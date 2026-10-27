package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	detapi "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 信号的即时通知（PRD 第5段 F7，用例5、41；channels.md 4.5；C-08）：
//   - 消费 detection.signal.detected.v1：每条信号记一行待展开的通知（收件人 '*'）。事件只是触发：信号、目标、处置与可见性在发送前都以
//     收件人本人重新读取，伪造或改写的事件不能让任何人看到其无权查看的内容；重放同一事件不新增通知（唯一键：租户 + 种类 + 信号 + 收件人）；
//   - 展开：以 channels 服务主体读取目标当前的生效策略（customer-context getActivePolicies，0.6.0 起带负责人与参与人），按 route 中该处置的
//     notify（mode 为 immediate 才展开）把受众 objective_owner / objective_members 展开为每名候选收件人一行；role:<角色> 受众尚不支持（C-20）；
//   - 每次发送（含重试）前按收件人当前状态依次再检查（任何一步之前不渲染、不签发令牌）：即时通知开关（最新偏好）→ 安静时段（延后到结束，
//     不丢弃）→ 成员有效 → 以收件人本人判断目标的 can_view_signals → 以收件人本人读取信号卡片（不可见、不属于该目标、不再待处理、回填）→
//     目标当前生效策略对信号当前处置仍为 immediate 且收件人仍在受众中 → 节流（每人每小时至多 6 张）→ 安静时段内到达、结束时简报即将发送的
//     并入简报（含本人已有含该信号的简报）→ 投递目标（未绑定为站内）→ 发送闸门（claimSignalSend，与简报保存互斥）。卡片与简报信号卡片同一模板与按钮（F3–F5）：第一次发送前渲染并固定（重试发送同一张卡片、令牌
//     不变），每次发送前按当前来源权利收紧后重新渲染正文；
//   - 投递事件 channels.signal_notice.sent.v1 / channels.signal_notice.failed.v1（与 channels.notice.* 同一语义，字段白名单见
//     signalNoticeEventData）；统计并入 getDeliveryStats 的 notices（种类 signal_immediate）。
//
// 组装简报时（Compose），本人已送达即时通知的信号标 notified、不再推送信号卡片；并入简报的与组装时尚未送达的即时通知标为
// merged_into_briefing（安静时段内到达的条目标“夜间到达”）。

// NoticeSignalImmediate 是信号即时通知的通知种类。
const NoticeSignalImmediate = "signal_immediate"

// DetectionSignalDetected 是即时通知消费的 detection 事件。
const DetectionSignalDetected = "detection.signal.detected.v1"

// 即时通知的投递事件（channels 事件 0.6.0）。
const (
	SignalNoticeSent   = "channels.signal_notice.sent.v1"
	SignalNoticeFailed = "channels.signal_notice.failed.v1"
)

// signalNoticeReasons 是即时通知各状态下允许出现在投递事件与投递统计中的原因（白名单；pending 与 delivered 不带原因）。
var signalNoticeReasons = map[string]map[string]bool{
	"retrying": {"channel_error": true},
	"in_app":   {"not_bound": true, "cutoff": true},
	"cancelled": {"member_inactive": true, "preference_off": true, "not_immediate": true, "not_in_audience": true, "objective_not_visible": true,
		"signal_not_visible": true, "signal_handled": true, "throttled": true, "merged_into_briefing": true},
}

// signalNoticeEventData 是即时通知投递事件的字段白名单：通知、种类、收件人、渠道、信号、目标与尝试数。不含项目名称、中标方、金额、命中理由
// 或卡片正文（来源字段的展示级别不进入事件）。
func signalNoticeEventData(id, recipient, signal, objective string, attempt int) map[string]any {
	return map[string]any{"notice_id": id, "kind": NoticeSignalImmediate, "recipient": recipient, "channel": Channel, "signal_id": signal,
		"objective_id": objective, "attempt": attempt}
}

// appendSignalNoticeFailed 发布一条 channels.signal_notice.failed.v1。未展开的行（收件人 '*'）不是一次投递，不发布；白名单外的原因不发布。
func appendSignalNoticeFailed(ctx context.Context, tx pgx.Tx, id, recipient, signal, objective string, attempt int, state, reason string) error {
	if recipient == "*" || !signalNoticeReasons[state][reason] {
		return nil
	}
	d := signalNoticeEventData(id, recipient, signal, objective, attempt)
	d["state"], d["reason"], d["final"] = state, reason, state != "retrying"
	_, err := writer.Append(ctx, tx, outbox.Event{Type: SignalNoticeFailed, Subject: id, PartitionKey: recipient, Data: d})
	return err
}

var (
	signalIDPattern    = regexp.MustCompile(`^sig_[0-9A-HJKMNP-TV-Z]{26}$`)
	objectiveIDPattern = regexp.MustCompile(`^obj_[0-9A-HJKMNP-TV-Z]{26}$`)
)

// signalDetectedEvent 是 detection.signal.detected.v1 中用到的字段（其余字段不保存）。
type signalDetectedEvent struct {
	SignalID    string    `json:"signal_id"`
	ObjectiveID string    `json:"objective_id"`
	Disposition string    `json:"disposition"`
	Backfill    bool      `json:"backfill"`
	CreatedAt   time.Time `json:"created_at"`
}

// signalNoticePayload 是即时通知行保存的事件内容（只有展开受众所需的处置；其余一律在发送前以收件人本人重新读取）。
type signalNoticePayload struct {
	Disposition string `json:"disposition"`
}

// SignalDetectedEvent 消费 detection.signal.detected.v1：排除信号与回填信号不即时通知（回填只在简报区头计数，F1 第7条）；其余每条信号记一行
// 待展开的通知。标识不合法的事件丢弃（不阻塞消费）；重放不新增。
func (s *Service) SignalDetectedEvent(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var data signalDetectedEvent
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return err
	}
	if env.Tenant == "" || env.Cell == "" {
		return fmt.Errorf("channels：信号事件 %s 缺少租户上下文", env.ID)
	}
	if !signalIDPattern.MatchString(data.SignalID) || !objectiveIDPattern.MatchString(data.ObjectiveID) {
		s.logger().WarnContext(ctx, "信号事件标识不合法，丢弃", "event", env.ID)
		return nil
	}
	if data.Disposition == "excluded" || data.Disposition == "" || data.Backfill {
		return nil
	}
	fire := data.CreatedAt
	if fire.IsZero() {
		fire = env.Time
	}
	payload, err := json.Marshal(signalNoticePayload{Disposition: data.Disposition})
	if err != nil {
		return err
	}
	now := s.businessNow()
	_, err = tx.Exec(ctx, `INSERT INTO chn_notices (id, tenant_id, kind, source, recipient, signal_id, objective_id, fire_at, payload, state, next_attempt_at, cutoff_at,
 created_at) VALUES ($1, $2, 'signal_immediate', $3, '*', $3, $4, $5, $6, 'pending', $7, $8, $7) ON CONFLICT (tenant_id, kind, source, recipient) DO NOTHING`,
		ids.New("dlv"), env.Tenant, data.SignalID, data.ObjectiveID, fire, payload, now, laterOf(now, fire).Add(NoticeCutoff))
	return err
}

// signalRoute 是目标当前生效策略对某一处置的通知设置与候选收件人。
type signalRoute struct {
	found, active, immediate bool
	code                     string
	audience                 []string
}

// routeDocument 是策略文档中 route 的通知部分（policy DSL 第3节；customer-context 合成文档时写入表单的通知覆盖）。
type routeDocument struct {
	Route map[string]struct {
		Notify *struct {
			Audience []string `json:"audience"`
			Mode     string   `json:"mode"`
		} `json:"notify"`
	} `json:"route"`
}

// signalRoute 以 channels 服务主体读取目标当前的生效策略，返回该处置的通知方式与候选收件人（负责人、参与人去重后按字典序）。
// 读取失败时返回错误（调用方稍后重试，不当作“不通知”）。目标不在生效策略中时 found 为假。
func (s *Service) signalRoute(sctx context.Context, objective, disposition string) (*signalRoute, error) {
	pages, err := s.activeObjectives(sctx)
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		for _, o := range p.Policies {
			if o.ObjectiveID != objective {
				continue
			}
			r := &signalRoute{found: true, active: o.ObjectiveState == "active", code: o.ObjectiveCode}
			raw, err := json.Marshal(&o.Document)
			if err != nil {
				return nil, err
			}
			var doc routeDocument
			if err := json.Unmarshal(raw, &doc); err != nil {
				return r, nil // 文档不可解析：不即时通知（失败关闭）
			}
			n := doc.Route[disposition].Notify
			if n == nil || n.Mode != "immediate" {
				return r, nil
			}
			r.immediate = true
			owner := string(o.Owner.Or(""))
			add := func(sub string) {
				if sub != "" && !slices.Contains(r.audience, sub) {
					r.audience = append(r.audience, sub)
				}
			}
			for _, a := range n.Audience {
				switch {
				case a == "objective_owner":
					add(owner)
				case a == "objective_members":
					add(owner)
					for _, p := range o.Participants {
						add(string(p))
					}
				}
				// role:<角色> 受众需要按目标单元枚举持有角色的成员（identity ListPermitted，C-07），尚不支持（C-20）
			}
			slices.Sort(r.audience)
			return r, nil
		}
	}
	return &signalRoute{}, nil
}

// expandSignalNotice 展开一条待展开的即时通知：按目标当前生效策略中信号处置（事件所记）的通知设置，为每名候选收件人记一行；不是 immediate、
// 目标未激活或不在生效策略中时不展开（0 人）。读取失败时稍后重试（不把未知当作不通知）。
func (s *Service) expandSignalNotice(sctx context.Context, n *noticeRow) {
	now := s.now().UTC()
	var payload signalNoticePayload
	_ = json.Unmarshal(n.payload, &payload)
	route, err := s.signalRoute(sctx, n.objective, payload.Disposition)
	if err != nil {
		s.rescheduleNotice(sctx, n.id, now.Add(time.Minute))
		return
	}
	var recipients []string
	mode := "digest"
	switch {
	case !route.found || !route.active:
		mode = "inactive"
	case route.immediate:
		mode, recipients = "immediate", route.audience
	}
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		added := 0
		for _, sub := range recipients {
			tag, err := tx.Exec(ctx, `INSERT INTO chn_notices (id, tenant_id, kind, source, recipient, signal_id, objective_id, fire_at, payload, state, next_attempt_at,
 cutoff_at, created_at) SELECT $2, tenant_id, kind, source, $3, signal_id, objective_id, fire_at, payload, 'pending', $4, cutoff_at, $4 FROM chn_notices
 WHERE id = $1 AND state IN ('pending', 'retrying') ON CONFLICT (tenant_id, kind, source, recipient) DO NOTHING`, n.id, ids.New("dlv"), sub, now)
			if err != nil {
				return err
			}
			added += int(tag.RowsAffected())
		}
		tag, err := tx.Exec(ctx, `UPDATE chn_notices SET state = 'expanded', expanded_recipients = $2, reason = 'recipients=' || $2::int
 WHERE id = $1 AND state IN ('pending', 'retrying')`, n.id, added)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return recordAs(ctx, tx, "channels.notice.expanded", n.id, "succeeded", fmt.Sprintf("wecom;signal=%s;route=%s;recipients=%d", n.signal, mode, added))
	})
	if err != nil {
		s.logger().WarnContext(sctx, "即时通知展开写入失败", "notice", n.id, "error", err.Error())
	}
}

// signalNoticeSource 是即时通知卡片的渲染来源：区标题、目标编号与条目快照（来源字段已按渲染时的展示级别白名单写入）。
type signalNoticeSource struct {
	Title string      `json:"title"`
	Code  string      `json:"code"`
	Item  *SignalItem `json:"item"`
}

// deliverSignalNotice 尝试一次即时通知的投递（见文件头的检查顺序）。依赖不可用时按退避重排，不把未知当作失去访问。
func (s *Service) deliverSignalNotice(sctx context.Context, rc reqctx.Context, n *noticeRow, now time.Time) {
	retry := func() { s.rescheduleNotice(sctx, n.id, now.Add(time.Minute)) }
	var sched domain.Schedule
	var current domain.Preferences
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if sched, err = s.schedule(ctx, tx, rc.Tenant, n.recipient); err != nil {
			return err
		}
		_, _, current, _ = sched.PeriodAt(now)
		if current.SendTime == "" {
			current = sched[len(sched)-1]
		}
		return nil
	})
	if err != nil {
		retry()
		return
	}
	// 即时通知开关按最新版本立即生效；安静时段按当期偏好，延后到结束（在任何读取、渲染与令牌签发之前）
	if !sched[len(sched)-1].InstantNotify {
		s.finishNotice(sctx, n, "cancelled", "preference_off")
		return
	}
	if until := domain.QuietUntil(current, now); !until.IsZero() {
		s.deferNotice(sctx, n, until)
		return
	}
	active, err := s.memberActive(sctx, n.recipient)
	if err != nil {
		retry()
		return
	}
	if !active {
		s.finishNotice(sctx, n, "cancelled", "member_inactive")
		return
	}
	// 以收件人本人再检查目标与信号（快照不固定权限）
	uctx := userContext(sctx, rc.Tenant, rc.Cell, n.recipient)
	visible, feedback, err := s.objectiveGrant(uctx, n.objective)
	if err != nil {
		retry()
		return
	}
	if !visible {
		s.finishNotice(sctx, n, "cancelled", "objective_not_visible")
		return
	}
	if s.Detection == nil {
		retry()
		return
	}
	cards, err := s.Detection.SignalCards(uctx, []string{n.signal})
	if err != nil {
		if dependencyTransient(err) {
			retry()
			return
		}
		s.finishNotice(sctx, n, "cancelled", "signal_not_visible")
		return
	}
	var card *detapi.SignalCard
	for i := range cards.Cards {
		if cards.Cards[i].Signal.ID == n.signal {
			card = &cards.Cards[i]
		}
	}
	switch {
	case card == nil || string(card.Signal.ObjectiveID) != n.objective:
		s.finishNotice(sctx, n, "cancelled", "signal_not_visible")
		return
	case card.Signal.Status != detapi.SignalStatusOpen:
		s.finishNotice(sctx, n, "cancelled", "signal_handled")
		return
	case card.Signal.Backfill:
		s.finishNotice(sctx, n, "cancelled", "not_immediate")
		return
	}
	// 目标当前生效策略对信号当前的系统处置（人工处置不触发 route，detection 第12节）仍为 immediate，且收件人仍在受众中
	route, err := s.signalRoute(sctx, n.objective, string(card.Signal.Disposition))
	if err != nil {
		retry()
		return
	}
	if !route.found || !route.active || !route.immediate {
		s.finishNotice(sctx, n, "cancelled", "not_immediate")
		return
	}
	if !slices.Contains(route.audience, n.recipient) {
		s.finishNotice(sctx, n, "cancelled", "not_in_audience")
		return
	}
	if n.attempts == 0 {
		// 节流与并入简报只在第一次发送前判断（已尝试过的同一张卡片照常重试）
		var recent int
		err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM chn_notices WHERE kind = 'signal_immediate' AND recipient = $1 AND state = 'delivered' AND accepted_at > $2`,
				n.recipient, now.Add(-domain.ImmediateWindow)).Scan(&recent)
		})
		if err != nil {
			retry()
			return
		}
		if recent >= domain.ImmediateLimit {
			s.finishNotice(sctx, n, "cancelled", "throttled")
			return
		}
		if n.fireAt != nil && domain.MergeIntoBriefing(current, *n.fireAt, now, sched.NextBoundary(now)) {
			s.finishNotice(sctx, n, "cancelled", "merged_into_briefing")
			return
		}
		// 本人已有含该信号的简报（简报先于展开组装）：并入，不渲染、不签发令牌。这里只是提前判断，发送闸门在锁下再判断一次
		var merged bool
		if err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			merged, err = mergeIntoComposed(ctx, tx, rc.Tenant, n)
			return err
		}); err != nil {
			retry()
			return
		}
		if merged {
			return
		}
	}
	target, err := s.DeliveryTarget(sctx, n.recipient)
	if err != nil {
		retry()
		return
	}
	if target.Channel != Channel {
		s.finishNotice(sctx, n, "in_app", "not_bound")
		return
	}
	// 第一次发送前渲染并固定（重试发送同一张卡片、令牌不变）；绑定换了（解绑后重新绑定）时按新绑定重新渲染
	if n.message == nil || n.item == nil || n.binding != target.BindingID {
		if err := s.renderSignalNotice(sctx, rc.Tenant, n, target.BindingID, card, feedback && card.ViewerCanFeedback, route.code, current); err != nil {
			if errors.Is(err, errNotBound) {
				s.finishNotice(sctx, n, "in_app", "not_bound")
				return
			}
			retry()
			return
		}
	}
	// 正文每次按当前来源权利收紧后重新渲染（按钮、令牌与链接不变；权利不可得时来源字段按 none）
	caps := s.provider().Capabilities()
	rights := s.fieldRights(sctx, []string{n.signal})
	it := restrictItem(n.item.Item, rights[n.signal])
	n.message.Card.Lines, _ = domain.FitCard(cardLines(n.item.Title, it, false, current.Location(), tenantLocation()), caps.LineMax, caps.LinesMax)
	pass, err := s.claimSignalSend(sctx, rc.Tenant, n)
	if err != nil {
		retry()
		return
	}
	if !pass {
		return
	}
	n.attempts++
	if err := s.provider().Send(sctx, n.corp, n.user, *n.message); err != nil {
		s.recordNotice(sctx, n, now, "retrying", now.Add(domain.Backoff(n.attempts)))
		return
	}
	s.recordNotice(sctx, n, now, "delivered", time.Time{})
}

// mergeIntoComposed 在收件人已有含该信号的简报（推送中、已送达或已在站内）时把尚未发送过的即时通知结束为 merged_into_briefing
// （发布结束事件、写审计），返回是否已并入。调用方保证通知尚未发送过；并发安全由发送闸门的咨询锁保证（见 claimSignalSend）。
func mergeIntoComposed(ctx context.Context, tx pgx.Tx, tenant string, n *noticeRow) (bool, error) {
	item, err := json.Marshal([]map[string]any{{"items": []map[string]any{{"kind": "signal", "signal_id": n.signal}}}})
	if err != nil {
		return false, err
	}
	var briefing string
	err = tx.QueryRow(ctx, `SELECT id FROM chn_briefings WHERE tenant_id = $1 AND subject = $2 AND push_state IN ('pushing', 'delivered', 'in_app')
 AND sections @> $3::jsonb ORDER BY assembled_at DESC, id LIMIT 1`, tenant, n.recipient, item).Scan(&briefing)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE chn_notices SET state = 'cancelled', reason = 'merged_into_briefing' WHERE id = $1 AND state IN ('pending', 'retrying')
 AND attempts = 0`, n.id)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if err := appendSignalNoticeFailed(ctx, tx, n.id, n.recipient, n.signal, n.objective, 0, "cancelled", "merged_into_briefing"); err != nil {
		return false, err
	}
	return true, recordAs(ctx, tx, "channels.notice.cancelled", n.id, "failed", "wecom;kind=signal_immediate;merged_into_briefing;briefing="+briefing)
}

// claimSignalSend 是即时通知的发送闸门，与组装简报的保存（saveBriefing）互斥：持有同一把收件人的简报咨询锁并锁定通知行，
//   - 通知已不是待发送（组装时已并入简报，或已被其他检查结束）时不发送；
//   - 第一次发送前（attempts 为 0），收件人已有含该信号的简报（推送中、已送达或已在站内）时取消为 merged_into_briefing，不发送——覆盖
//     简报先于展开组装的顺序：组装时收件人 '*' 的行尚未展开，简报照常推送该信号；
//   - 否则在同一事务中把 attempts 加一再发送：之后组装的简报把该信号标“已通知”、不再推送信号卡片；已读取旧状态、尚未保存的组装在保存时
//     发现状态变化而重做（errNoticeRaced）。
//
// 两条路径都在同一把锁下“检查后写入”，因此无论先后与并发，同一信号对同一收件人只会以即时卡片或简报卡片之一送达。进程在闸门之后、记录结果
// 之前退出时，attempts 已加一：之后的简报按“已通知”处理，租约过期后照常重试同一张卡片。
func (s *Service) claimSignalSend(sctx context.Context, tenant string, n *noticeRow) (bool, error) {
	pass := false
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "chn-briefing/"+tenant+"/"+n.recipient); err != nil {
			return err
		}
		var state string
		var attempts int
		err := tx.QueryRow(ctx, `SELECT state, attempts FROM chn_notices WHERE id = $1 FOR UPDATE`, n.id).Scan(&state, &attempts)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && state != "pending" && state != "retrying" {
			return nil
		}
		if err != nil {
			return err
		}
		if attempts == 0 {
			merged, err := mergeIntoComposed(ctx, tx, tenant, n)
			if err != nil || merged {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE chn_notices SET attempts = attempts + 1 WHERE id = $1`, n.id); err != nil {
			return err
		}
		pass = true
		return nil
	})
	return pass, err
}

// renderSignalNotice 按收件人本人读取的信号卡片渲染即时通知卡片（与简报信号卡片同一模板与按钮），签发令牌，并与投递目标一起固定在通知上。
func (s *Service) renderSignalNotice(sctx context.Context, tenant string, n *noticeRow, binding string, card *detapi.SignalCard, feedback bool, code string,
	prefs domain.Preferences) error {
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
	title := code
	if s.Packs != nil {
		if sec, ok := s.Packs.Sections[code]; ok {
			title = sec.Title
		}
	}
	rights := s.fieldRights(sctx, []string{n.signal})
	it := signalFields(detection.BriefingSignal{Signal: card.Signal, Category: detapi.BriefingSignalCategoryNew, Updates: []detapi.SignalUpdate{}}, rights[n.signal])
	m := &minter{s: s, tenant: tenant, recipient: n.recipient, now: s.businessNow()}
	m.pursue = s.pursueGrants(sctx, tenant, n.recipient, []string{n.objective})
	msg, err := m.signalCard(code, title, n.objective, it, feedback, prefs)
	if err != nil {
		return err
	}
	msg.Text = "即时通知：" + title + " · " + dispositionLabel[it.Disposition]
	src := signalNoticeSource{Title: title, Code: code, Item: it}
	rawMsg, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	rawSrc, err := json.Marshal(src)
	if err != nil {
		return err
	}
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertTokens(ctx, tx, tenant, "", m.tokens, m.now); err != nil {
			return err
		}
		if err := insertLinks(ctx, tx, tenant, m.links, m.now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE chn_notices SET binding_id = $2, corp_id = $3, channel_user_id = $4, message = $5, item = $6 WHERE id = $1`,
			n.id, binding, corp, user, rawMsg, rawSrc)
		return err
	})
	if err != nil {
		return err
	}
	n.binding, n.corp, n.user, n.message, n.item = binding, corp, user, &msg, &src
	return nil
}

// noticeMarks 是组装简报时本人对已列入条目的信号的即时通知：notified 为已送达或已过发送闸门（attempts > 0：发送中或渠道失败待重试，本期
// 不再推送信号卡片），pending 为尚未发送过（随本期简报结束，标为 merged_into_briefing）；overnight 为到达时在安静时段内（条目标“夜间到达”）。
// 收件人为 '*' 的未展开行不在此列：展开后的每一条在发送闸门处发现本人已有含该信号的简报而并入（见 claimSignalSend）。
type noticeMarks struct {
	notified, overnight map[string]bool
	pending             []noticeRow
}

// signalNoticeMarks 读取收件人对给定信号的即时通知。
func (s *Service) signalNoticeMarks(sctx context.Context, subject string, signals []string, prefs domain.Preferences) (*noticeMarks, error) {
	out := &noticeMarks{notified: map[string]bool{}, overnight: map[string]bool{}}
	if len(signals) == 0 {
		return out, nil
	}
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, signal_id, objective_id, state, COALESCE(reason, ''), fire_at, attempts FROM chn_notices
 WHERE kind = 'signal_immediate' AND recipient = $1 AND signal_id = ANY($2) AND (state IN ('pending', 'retrying', 'delivered')
 OR (state = 'cancelled' AND reason = 'merged_into_briefing'))`, subject, signals)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n noticeRow
			var state, reason string
			if err := rows.Scan(&n.id, &n.signal, &n.objective, &state, &reason, &n.fireAt, &n.attempts); err != nil {
				return err
			}
			n.kind, n.recipient = NoticeSignalImmediate, subject
			arrivedQuiet := n.fireAt != nil && !domain.QuietUntil(prefs, *n.fireAt).IsZero()
			switch {
			case state == "delivered" || n.attempts > 0:
				// 已送达，或已过发送闸门（发送中或渠道失败待重试）：本期不再推送信号卡片
				out.notified[n.signal] = true
			case state == "cancelled":
				out.overnight[n.signal] = out.overnight[n.signal] || arrivedQuiet
			default:
				out.pending = append(out.pending, n)
				out.overnight[n.signal] = out.overnight[n.signal] || arrivedQuiet
			}
		}
		return rows.Err()
	})
	return out, err
}

// applyNoticeMarks 把即时通知的标记写入条目（只标收件人本人的、完整展示的信号条目）。
func applyNoticeMarks(secs []composedSection, marks *noticeMarks) {
	for si := range secs {
		for ii := range secs[si].Items {
			it := &secs[si].Items[ii]
			if it.Kind != "signal" || it.SignalItem == nil {
				continue
			}
			if marks.notified[it.SignalID] {
				it.Notified = true
				continue
			}
			if marks.overnight[it.SignalID] {
				it.ArrivedOvernight = true
			}
		}
	}
}

// errNoticeRaced 表示组装读取即时通知之后、保存简报之前，本人有即时通知过了发送闸门：组装按新状态重做（Compose 内重试）。
var errNoticeRaced = errors.New("channels：组装期间即时通知已发送")

// mergePendingNotices 在保存简报的事务中（已持有收件人的简报咨询锁，与发送闸门互斥）先确认组装时读取的即时通知状态仍然成立：本期列出的信号中，
// 有已送达或已过发送闸门、而组装时未标“已通知”的，返回 errNoticeRaced（否则同一信号会既有即时卡片又有简报卡片）；之后结束本期已列入、
// 尚未发送过的即时通知（merged_into_briefing，发布结束事件），简报之后不再单独推送。
func mergePendingNotices(ctx context.Context, tx pgx.Tx, briefing, subject string, listed []string, marks *noticeMarks) error {
	if marks == nil || len(listed) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT signal_id FROM chn_notices WHERE kind = 'signal_immediate' AND recipient = $1 AND signal_id = ANY($2)
 AND (state = 'delivered' OR (state IN ('pending', 'retrying') AND attempts > 0))`, subject, listed)
	if err != nil {
		return err
	}
	sent, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, sig := range sent {
		if !marks.notified[sig] {
			return errNoticeRaced
		}
	}
	for i := range marks.pending {
		n := &marks.pending[i]
		tag, err := tx.Exec(ctx, `UPDATE chn_notices SET state = 'cancelled', reason = 'merged_into_briefing' WHERE id = $1 AND state IN ('pending', 'retrying')
 AND attempts = 0`, n.id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if err := appendSignalNoticeFailed(ctx, tx, n.id, n.recipient, n.signal, n.objective, n.attempts, "cancelled", "merged_into_briefing"); err != nil {
			return err
		}
		if err := recordAs(ctx, tx, "channels.notice.cancelled", n.id, "failed", "wecom;kind=signal_immediate;merged_into_briefing;briefing="+briefing); err != nil {
			return err
		}
	}
	return nil
}

// listedSignals 返回各区完整展示的信号条目。
func listedSignals(secs []composedSection) []string {
	var out []string
	for _, sec := range secs {
		for _, it := range sec.Items {
			if it.Kind == "signal" && it.SignalItem != nil {
				out = append(out, it.SignalID)
			}
		}
	}
	return out
}
