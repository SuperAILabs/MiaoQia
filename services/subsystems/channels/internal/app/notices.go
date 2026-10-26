package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	purapi "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
)

// 渠道通知（PRD 第6段 F9 第2–3条、F12 第4、6条、第13节第11项；C-09）：
//   - 任务提醒卡片：消费 pursuit.task.due.v1，给收件人（任务负责人）发一张带“完成”与“查看”的卡片；转给决策者的提醒（escalated，
//     事件不带收件人）在投递时展开：以每个已绑定成员本人经 pursuit getTask 判断 viewer_escalated，展开为各自的一行（未绑定的决策者
//     在简报“待我处理”与 Web 中看到）；
//   - 授权被拒的通知：消费 pursuit.pursuit.authorization_rejected.v1，事件带操作者（最近一次变更团队的成员）时通知操作者。
//
// 快照不固定权限：每次发送（含重试）前按收件人当前状态再检查——成员有效、投递目标（未绑定为站内）、以收件人本人经 pursuit 读取的任务
// 状态、截止版本、负责人或转交关系与写入准入（授权同步中延后），以及同一任务是否已有更新的提醒（只发最新的一条）；不满足的取消。
// 安静时段内延后到结束（不丢弃，F2 第2条与第6段第13节第11项）；创建后 NoticeCutoff 内未送达的停止渠道重试，留在 Web 与站内。

// NoticeCutoff 是通知的渠道重试时限（从事件对应的时刻起）。
const NoticeCutoff = 24 * time.Hour

// noticeFanout 是一条转给决策者的提醒最多展开的已绑定成员数。
const noticeFanout = 500

// 渠道通知消费的 pursuit 事件。
const (
	PursuitTaskDue               = "pursuit.task.due.v1"
	PursuitAuthorizationRejected = "pursuit.pursuit.authorization_rejected.v1"
)

// taskDueEvent 是 pursuit.task.due.v1 的数据（只取用到的字段）。
type taskDueEvent struct {
	ReminderID      string          `json:"reminder_id"`
	TaskID          string          `json:"task_id"`
	PursuitID       string          `json:"pursuit_id"`
	ObjectiveID     string          `json:"objective_id"`
	Kind            string          `json:"kind"`
	DeadlineVersion int64           `json:"deadline_version"`
	Due             json.RawMessage `json:"due,omitempty"`
	Escalated       bool            `json:"escalated"`
	Recipient       string          `json:"recipient,omitempty"`
	FireAt          time.Time       `json:"fire_at"`
}

// rejectedEvent 是 pursuit.pursuit.authorization_rejected.v1 的数据。
type rejectedEvent struct {
	PursuitID            string `json:"pursuit_id"`
	ObjectiveID          string `json:"objective_id"`
	ParticipationVersion int64  `json:"participation_version"`
	Rejected             []struct {
		Subject  string `json:"subject"`
		Relation string `json:"relation"`
		Reason   string `json:"reason"`
	} `json:"rejected"`
	OwnerInvalid bool      `json:"owner_invalid"`
	Operator     string    `json:"operator,omitempty"`
	RecordedAt   time.Time `json:"recorded_at"`
}

// TaskDueEvent 消费 pursuit.task.due.v1：每个（提醒、收件人）记一条待发送的通知。乱序到达按（截止版本、提醒时刻）比较：同一任务已有
// 更新的通知时本条记为已取消（superseded），否则取消同一任务更早的未发送通知（只发最新的一条）。
func (s *Service) TaskDueEvent(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data taskDueEvent
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	if env.Tenant == "" || env.Cell == "" {
		return fmt.Errorf("channels：提醒事件 %s 缺少租户上下文", env.ID)
	}
	recipient := data.Recipient
	if data.Escalated || recipient == "" {
		recipient = "*"
	}
	if data.FireAt.IsZero() {
		data.FireAt = env.Time
	}
	var newer int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM chn_notices WHERE kind = 'task_due' AND task_id = $1 AND source <> $2
 AND (deadline_version > $3 OR (deadline_version = $3 AND fire_at > $4))`, data.TaskID, data.ReminderID, data.DeadlineVersion, data.FireAt).Scan(&newer); err != nil {
		return err
	}
	state, reason := "pending", (*string)(nil)
	if newer > 0 {
		sup := "superseded"
		state, reason = "cancelled", &sup
	} else if _, err := tx.Exec(ctx, `UPDATE chn_notices SET state = 'cancelled', reason = 'superseded' WHERE kind = 'task_due' AND task_id = $1
 AND state IN ('pending', 'retrying') AND (deadline_version < $2 OR (deadline_version = $2 AND fire_at < $3))`, data.TaskID, data.DeadlineVersion, data.FireAt); err != nil {
		return err
	}
	now := s.businessNow()
	_, err := tx.Exec(ctx, `INSERT INTO chn_notices (id, tenant_id, kind, source, recipient, pursuit_id, task_id, deadline_version, escalated, fire_at, payload, state,
 reason, next_attempt_at, cutoff_at, created_at) VALUES ($1, $2, 'task_due', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $13)
 ON CONFLICT (tenant_id, kind, source, recipient) DO NOTHING`,
		ids.New("dlv"), env.Tenant, data.ReminderID, recipient, data.PursuitID, data.TaskID, data.DeadlineVersion, recipient == "*", data.FireAt, env.Data, state,
		reason, now, laterOf(now, data.FireAt).Add(NoticeCutoff))
	return err
}

// AuthorizationRejectedEvent 消费 pursuit.pursuit.authorization_rejected.v1：事件带操作者时给操作者记一条待发送的通知（每个跟进的
// 每个参与版本一条）。不带操作者（系统再校验触发）时不通知。
func (s *Service) AuthorizationRejectedEvent(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data rejectedEvent
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	if env.Tenant == "" || env.Cell == "" {
		return fmt.Errorf("channels：授权被拒事件 %s 缺少租户上下文", env.ID)
	}
	if data.Operator == "" || strings.HasPrefix(data.Operator, "svc:") {
		return nil
	}
	now := s.businessNow()
	_, err := tx.Exec(ctx, `INSERT INTO chn_notices (id, tenant_id, kind, source, recipient, pursuit_id, fire_at, payload, state, next_attempt_at, cutoff_at, created_at)
 VALUES ($1, $2, 'authorization_rejected', $3, $4, $5, $6, $7, 'pending', $8, $9, $8) ON CONFLICT (tenant_id, kind, source, recipient) DO NOTHING`,
		ids.New("dlv"), env.Tenant, fmt.Sprintf("%s/%d", data.PursuitID, data.ParticipationVersion), data.Operator, data.PursuitID, data.RecordedAt, env.Data,
		now, laterOf(now, data.RecordedAt).Add(NoticeCutoff))
	return err
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// DeliverNotices 是 Notices.DeliverNotices：发送到时的渠道通知（含安静时段结束后的延后通知与退避重试），返回处理的条数。
func (s *Service) DeliverNotices(ctx context.Context, limit int) (int, error) {
	rc, err := requireService(ctx)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	sctx := serviceContext(ctx, rc.Tenant, rc.Cell)
	var due []string
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM chn_notices WHERE state IN ('pending', 'retrying') AND next_attempt_at <= $1 ORDER BY next_attempt_at, id LIMIT $2`,
			s.now(), limit)
		if err != nil {
			return err
		}
		due, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return 0, err
	}
	for _, id := range due {
		s.deliverNotice(sctx, id)
	}
	return len(due), nil
}

type noticeRow struct {
	id, kind, source, recipient, pursuit, task, binding, corp, user string
	deadline                                                        int64
	escalated                                                       bool
	fireAt                                                          *time.Time
	payload                                                         []byte
	message                                                         *api.Message
	attempts                                                        int
	cutoff                                                          time.Time
}

// deliverNotice 尝试一次通知投递（以租约认领，见文件头）。依赖不可用时按退避重排，不把未知当作失去访问。
func (s *Service) deliverNotice(sctx context.Context, id string) {
	rc, _ := requireService(sctx)
	now := s.now().UTC()
	var n noticeRow
	claimed := false
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		var task, binding, corp, user *string
		var dv *int64
		var msg []byte
		err := tx.QueryRow(ctx, `UPDATE chn_notices SET next_attempt_at = $2 WHERE id = $1 AND state IN ('pending', 'retrying') AND next_attempt_at <= $3
 RETURNING kind, source, recipient, pursuit_id, task_id, deadline_version, escalated, fire_at, payload, binding_id, corp_id, channel_user_id, message, attempts, cutoff_at`,
			id, now.Add(2*time.Minute), now).Scan(&n.kind, &n.source, &n.recipient, &n.pursuit, &task, &dv, &n.escalated, &n.fireAt, &n.payload, &binding, &corp, &user,
			&msg, &n.attempts, &n.cutoff)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		n.id, claimed = id, true
		for _, p := range []struct {
			src *string
			dst *string
		}{{task, &n.task}, {binding, &n.binding}, {corp, &n.corp}, {user, &n.user}} {
			if p.src != nil {
				*p.dst = *p.src
			}
		}
		if dv != nil {
			n.deadline = *dv
		}
		if msg != nil {
			n.message = &api.Message{}
			return json.Unmarshal(msg, n.message)
		}
		return nil
	})
	if err != nil || !claimed {
		if err != nil {
			s.logger().WarnContext(sctx, "通知认领失败", "notice", id, "error", err.Error())
		}
		return
	}
	retry := func() { s.rescheduleNotice(sctx, n.id, now.Add(time.Minute)) }
	if !now.Before(n.cutoff) {
		s.finishNotice(sctx, &n, "in_app", "cutoff")
		return
	}
	if n.recipient == "*" {
		s.expandNotice(sctx, rc, &n)
		return
	}
	// 安静时段内延后到结束（不丢弃）：在任何渲染与令牌签发之前判断
	var prefs domain.Preferences
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		sched, err := s.schedule(ctx, tx, rc.Tenant, n.recipient)
		if err == nil {
			_, _, prefs, _ = sched.PeriodAt(now)
			if prefs.SendTime == "" {
				prefs = sched[len(sched)-1]
			}
		}
		return err
	})
	if err != nil {
		retry()
		return
	}
	if until := domain.QuietUntil(prefs, now); !until.IsZero() {
		s.deferNotice(sctx, &n, until)
		return
	}
	active, err := s.memberActive(sctx, n.recipient)
	if err != nil {
		retry()
		return
	}
	if !active {
		s.finishNotice(sctx, &n, "cancelled", "member_inactive")
		return
	}
	uctx := userContext(sctx, rc.Tenant, rc.Cell, n.recipient)
	var view *purapi.TaskView
	var pursuitView *purapi.Pursuit
	if s.Pursuits == nil {
		retry()
		return
	}
	// 以收件人本人经 pursuit 再检查（快照不固定权限）
	switch n.kind {
	case "task_due":
		v, err := s.Pursuits.GetTask(uctx, n.task)
		if err != nil {
			if dependencyTransient(err) {
				retry()
				return
			}
			s.finishNotice(sctx, &n, "cancelled", "task_not_visible")
			return
		}
		switch {
		case v.PursuitState != purapi.PursuitStatusOpen || v.Task.Status != purapi.TaskStatusTodo:
			s.finishNotice(sctx, &n, "cancelled", "task_closed")
			return
		case v.Task.DeadlineVersion != n.deadline:
			s.finishNotice(sctx, &n, "cancelled", "superseded")
			return
		case !(v.ViewerIsAssignee && !n.escalated) && !(v.ViewerEscalated.Or(false) && n.escalated):
			s.finishNotice(sctx, &n, "cancelled", "reassigned")
			return
		case !v.ViewerCanEdit:
			retry() // 授权同步中：同步完成后再发（F12 第4条）
			return
		}
		var newer int
		err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM chn_notices WHERE kind = 'task_due' AND task_id = $1 AND id <> $2 AND state <> 'cancelled'
 AND (deadline_version > $3 OR (deadline_version = $3 AND fire_at > $4))`, n.task, n.id, n.deadline, n.fireAt).Scan(&newer)
		})
		if err != nil {
			retry()
			return
		}
		if newer > 0 {
			s.finishNotice(sctx, &n, "cancelled", "superseded")
			return
		}
		view = v
	case "authorization_rejected":
		p, err := s.Pursuits.GetPursuit(uctx, n.pursuit)
		if err != nil {
			if dependencyTransient(err) {
				retry()
				return
			}
			s.finishNotice(sctx, &n, "cancelled", "pursuit_not_visible")
			return
		}
		pursuitView = p
	}
	target, err := s.DeliveryTarget(sctx, n.recipient)
	if err != nil {
		retry()
		return
	}
	if target.Channel != Channel {
		s.finishNotice(sctx, &n, "in_app", "not_bound")
		return
	}
	// 第一次发送前渲染并固定（重试发送同一张卡片、令牌不变）；绑定换了（解绑后重新绑定）时按新绑定重新渲染
	if n.message == nil || n.binding != target.BindingID {
		if err := s.renderNotice(sctx, rc.Tenant, &n, target.BindingID, view, pursuitView); err != nil {
			if errors.Is(err, errNotBound) {
				s.finishNotice(sctx, &n, "in_app", "not_bound")
				return
			}
			retry()
			return
		}
	}
	// 正文每次按当前值重新渲染（按钮与令牌不变）
	caps := s.provider().Capabilities()
	if view != nil {
		n.message.Card.Lines, _ = fitLines(taskLines(view, n.escalated, now), caps)
	} else if pursuitView != nil {
		n.message.Card.Lines, _ = fitLines(rejectedLines(n.payload, pursuitView), caps)
	}
	n.attempts++
	if err := s.provider().Send(sctx, n.corp, n.user, *n.message); err != nil {
		s.recordNotice(sctx, &n, now, "retrying", now.Add(domain.Backoff(n.attempts)))
		return
	}
	s.recordNotice(sctx, &n, now, "delivered", time.Time{})
}

var errNotBound = errors.New("not_bound")

// expandNotice 展开一条转给决策者的提醒：以每个已绑定的有效成员本人经 pursuit getTask 判断 viewer_escalated（与 listMyTasks 同一口径），
// 为每个满足的成员记一条待发送的通知；任何判断不可用时整体重排（不把未知当作不是决策者）。
func (s *Service) expandNotice(sctx context.Context, rc reqctx.Context, n *noticeRow) {
	now := s.now().UTC()
	if s.Pursuits == nil {
		s.rescheduleNotice(sctx, n.id, now.Add(time.Minute))
		return
	}
	var subjects []string
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT subject FROM chn_bindings WHERE state = 'bound' ORDER BY subject LIMIT $1`, noticeFanout)
		if err != nil {
			return err
		}
		subjects, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		s.rescheduleNotice(sctx, n.id, now.Add(time.Minute))
		return
	}
	var picked []string
	for _, sub := range subjects {
		v, err := s.Pursuits.GetTask(userContext(sctx, rc.Tenant, rc.Cell, sub), n.task)
		if err != nil {
			if dependencyTransient(err) {
				s.rescheduleNotice(sctx, n.id, now.Add(time.Minute))
				return
			}
			continue
		}
		if v.ViewerEscalated.Or(false) && v.Task.DeadlineVersion == n.deadline && v.Task.Status == purapi.TaskStatusTodo {
			picked = append(picked, sub)
		}
	}
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE chn_notices SET state = 'expanded', reason = $2 WHERE id = $1 AND state IN ('pending', 'retrying')`, n.id,
			fmt.Sprintf("recipients=%d", len(picked)))
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		for _, sub := range picked {
			if _, err := tx.Exec(ctx, `INSERT INTO chn_notices (id, tenant_id, kind, source, recipient, pursuit_id, task_id, deadline_version, escalated, fire_at, payload,
 state, next_attempt_at, cutoff_at, created_at) SELECT $2, tenant_id, kind, source, $3, pursuit_id, task_id, deadline_version, true, fire_at, payload, 'pending', $4, cutoff_at, $4
 FROM chn_notices WHERE id = $1 ON CONFLICT (tenant_id, kind, source, recipient) DO NOTHING`, n.id, ids.New("dlv"), sub, now); err != nil {
				return err
			}
		}
		return recordAs(ctx, tx, "channels.notice.expanded", n.id, "succeeded", fmt.Sprintf("wecom;task=%s;recipients=%d", n.task, len(picked)))
	})
	if err != nil {
		s.logger().WarnContext(sctx, "通知展开写入失败", "notice", n.id, "error", err.Error())
	}
}

// renderNotice 渲染通知卡片、签发令牌，并与投递目标一起固定在通知上。
func (s *Service) renderNotice(sctx context.Context, tenant string, n *noticeRow, binding string, view *purapi.TaskView, p *purapi.Pursuit) error {
	var corp, user string
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT corp_id, channel_user_id FROM chn_bindings WHERE id = $1 AND state = 'bound'`, binding).Scan(&corp, &user)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotBound
	}
	if err != nil {
		return err
	}
	caps := s.provider().Capabilities()
	card, err := newCardID()
	if err != nil {
		return err
	}
	now := s.businessNow()
	m := &minter{s: s, tenant: tenant, recipient: n.recipient, now: now}
	var msg api.Message
	switch {
	case view != nil:
		done, err := m.taskToken(n.task, string(view.ObjectiveID), "complete_task", card, "", n.deadline, domain.ActionTokenTTL)
		if err != nil {
			return err
		}
		title := "任务提醒：" + view.Task.Title
		if n.escalated {
			title = "待你决定的任务：" + view.Task.Title
		}
		lines, _ := fitLines(taskLines(view, n.escalated, now), caps)
		msg = api.Message{Kind: "task_reminder", Card: &api.Card{CardID: card, Title: domain.Truncate(title, caps.TitleMax), Lines: lines, Buttons: []api.Button{
			{Key: "complete", Label: "完成", Token: done}, {Key: "view", Label: "查看", URL: taskURL(n.task)}}}}
	case p != nil:
		lines, _ := fitLines(rejectedLines(n.payload, p), caps)
		msg = api.Message{Kind: "authorization_rejected", Card: &api.Card{CardID: card, Title: domain.Truncate("跟进成员未获授权", caps.TitleMax), Lines: lines,
			Buttons: []api.Button{{Key: "view", Label: "查看", URL: pursuitURL(n.pursuit)}}}}
	default:
		return fmt.Errorf("channels：通知 %s 没有渲染来源", n.id)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertTokens(ctx, tx, tenant, "", m.tokens, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE chn_notices SET binding_id = $2, corp_id = $3, channel_user_id = $4, message = $5 WHERE id = $1`, n.id, binding, corp, user, raw)
		return err
	})
	if err != nil {
		return err
	}
	n.binding, n.corp, n.user, n.message = binding, corp, user, &msg
	return nil
}

// fitLines 按渠道能力放入卡片正文：第一行保留，其余按顺序为优先级（靠后的先省略）。
func fitLines(lines []string, caps api.Capabilities) ([]string, int) {
	out := make([]domain.CardLine, len(lines))
	for i, l := range lines {
		out[i] = domain.CardLine{Text: l, Keep: i == 0, Priority: i}
	}
	return domain.FitCard(out, caps.LineMax, caps.LinesMax)
}

// taskLines 渲染任务提醒卡片的正文（当前值）。
func taskLines(v *purapi.TaskView, escalated bool, now time.Time) []string {
	var lines []string
	if due, ok := v.Task.Due.Get(); ok {
		text := "截止 " + due.Value
		if due.Timezone != "" {
			text += "（" + due.Timezone + "）"
		}
		lines = append(lines, text)
	}
	if at, ok := v.Task.DueAt.Get(); ok && now.After(at) {
		lines = append(lines, "已逾期")
	}
	if escalated {
		lines = append(lines, "负责人已不能处理，转给你决定（可完成、改派或在 Web 中处理）")
	}
	if note, ok := v.Task.Note.Get(); ok && note != "" {
		lines = append(lines, note)
	}
	return lines
}

// rejectedLines 渲染授权被拒通知的正文：被移出的人数（按负责人、协作者）与原因，负责人失效时说明任务转给决策者。
func rejectedLines(payload []byte, p *purapi.Pursuit) []string {
	var data rejectedEvent
	_ = json.Unmarshal(payload, &data)
	owners, collaborators := 0, 0
	reasons := map[string]bool{}
	var order []string
	for _, r := range data.Rejected {
		if r.Relation == "owner" {
			owners++
		} else {
			collaborators++
		}
		if !reasons[r.Reason] {
			reasons[r.Reason] = true
			order = append(order, r.Reason)
		}
	}
	lines := []string{fmt.Sprintf("你最近设置的团队中有 %d 人未获授权，已移出（负责人 %d、协作者 %d）", owners+collaborators, owners, collaborators)}
	if len(order) > 0 {
		lines = append(lines, "原因："+strings.Join(order, "、"))
	}
	if data.OwnerInvalid && p != nil && !p.OwnerValid {
		lines = append(lines, "负责人已失效，待办任务转给单元决策者处理，请在 Web 中重新指定负责人")
	}
	return lines
}

func (s *Service) rescheduleNotice(sctx context.Context, id string, at time.Time) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE chn_notices SET next_attempt_at = $2 WHERE id = $1 AND state IN ('pending', 'retrying')`, id, at)
		return err
	})
	if err != nil {
		s.logger().WarnContext(sctx, "通知重排失败", "notice", id, "error", err.Error())
	}
}

// deferNotice 把安静时段内到时的通知延后到安静时段结束（不丢弃）；渠道重试时限顺延到结束之后。
func (s *Service) deferNotice(sctx context.Context, n *noticeRow, until time.Time) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE chn_notices SET next_attempt_at = $2, cutoff_at = GREATEST(cutoff_at, $3) WHERE id = $1 AND state IN ('pending', 'retrying')`,
			n.id, until, until.Add(NoticeCutoff))
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return recordAs(ctx, tx, "channels.notice.deferred", n.id, "succeeded", "wecom;quiet_hours;until="+until.UTC().Format(time.RFC3339))
	})
	if err != nil {
		s.logger().WarnContext(sctx, "通知延后失败", "notice", n.id, "error", err.Error())
	}
}

// recordNotice 记录一次发送尝试的结果并写审计。
func (s *Service) recordNotice(sctx context.Context, n *noticeRow, now time.Time, state string, next time.Time) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		if state == "delivered" {
			tag, err := tx.Exec(ctx, `UPDATE chn_notices SET state = 'delivered', reason = NULL, attempts = $2, submitted_at = $3, accepted_at = $3
 WHERE id = $1 AND state IN ('pending', 'retrying')`, n.id, n.attempts, now)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			return recordAs(ctx, tx, "channels.notice.delivered", n.id, "succeeded", fmt.Sprintf("wecom;kind=%s;attempt=%d", n.kind, n.attempts))
		}
		tag, err := tx.Exec(ctx, `UPDATE chn_notices SET state = 'retrying', reason = 'channel_error', attempts = $2, submitted_at = $3, next_attempt_at = $4
 WHERE id = $1 AND state IN ('pending', 'retrying')`, n.id, n.attempts, now, next)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return recordAs(ctx, tx, "channels.notice.retrying", n.id, "failed", fmt.Sprintf("wecom;kind=%s;attempt=%d", n.kind, n.attempts))
	})
	if err != nil {
		s.logger().WarnContext(sctx, "通知结果写入失败", "notice", n.id, "error", err.Error())
	}
}

// finishNotice 结束通知：in_app（未绑定或截止后仍未送达）或 cancelled（发送前再检查时取消）。
func (s *Service) finishNotice(sctx context.Context, n *noticeRow, state, reason string) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE chn_notices SET state = $2, reason = $3 WHERE id = $1 AND state IN ('pending', 'retrying')`, n.id, state, reason)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return recordAs(ctx, tx, "channels.notice."+state, n.id, "failed", "wecom;kind="+n.kind+";"+reason)
	})
	if err != nil {
		s.logger().WarnContext(sctx, "通知结束写入失败", "notice", n.id, "error", err.Error())
	}
}
