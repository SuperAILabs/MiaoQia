package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	purapi "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
	pursuitapi "github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 任务提醒卡片上的“完成”与“撤销”（PRD 第6段 F9 第2–3条、第13节第11项；C-09）。点击沿用第5段的回调、令牌与执行检查（签名、租户、卡片、
// 点击者的有效绑定与成员资格、点击者是收件人、已固定的结果、到期），之后：
//   - 以点击者本人经 pursuit getTask 再检查：已不能查看任务为确定失败 forbidden；“完成”的令牌所在截止版本已不是任务当前的截止版本
//     （截止改变后重建的提醒是新卡片与新令牌）为确定失败 deadline_changed；点击者已不是任务负责人、也不是转交给他的决策者为确定
//     失败 forbidden（“已不是负责人”）。pursuit 的写入准入（授权同步、授予版本）由 pursuit 按点击者判断，channels 不另作判断；
//   - 以点击者的用户主体调用 pursuit completeTask / reopenTask（channel 为 wecom），操作标识为令牌标识，每次尝试的幂等键为
//     “令牌标识-序号”；“完成”的请求（所见任务版本）在首次执行时固定在令牌上，之后每次尝试发送同一请求；
//   - 成功、无变化与确定失败固定在令牌上并发布 channels.task_action.executed.v1；暂时失败与 authorization_syncing（pursuit 不固定在
//     操作上，同步完成后同一操作可成功）不作废令牌，只写审计。成功的“完成”带“撤销”（10 分钟，pursuit 的渠道撤销时限）。

// TaskActionExecuted 是任务提醒卡片动作的结果事件。
const TaskActionExecuted = "channels.task_action.executed.v1"

// taskURL / pursuitURL 是 Web 中任务与跟进详情的地址（只指向对象、不带令牌；打开时由 pursuit 以当前登录成员授权）。
func taskURL(task string) string       { return "/pursuits/tasks/" + task }
func pursuitURL(pursuit string) string { return "/pursuits/" + pursuit }

// trackingURL 是持续跟踪的 Web 详情地址（只指向对象、不带令牌，打开时由 pursuit 以当前登录成员授权；C-11/C-12）。
func trackingURL(tracking string) string { return "/pursuits/tracking/" + tracking }

// taskToken 签发任务提醒卡片上的动作令牌（令牌标识即操作标识）。
func (m *minter) taskToken(task, objective, action, card, targetOp string, deadline int64, ttl time.Duration) (string, error) {
	t := tokenRow{id: ids.New("atk"), recipient: m.recipient, objective: objective, action: action, card: card, task: task, deadline: deadline,
		targetOp: targetOp, expires: m.now.Add(ttl)}
	sealed, err := m.s.seal(purposeAction, actionClaims{ID: t.id, Tenant: m.tenant, Recipient: t.recipient, Action: action, Card: card, Expires: t.expires,
		Task: task, Deadline: deadline, Target: targetOp})
	if err != nil {
		return "", err
	}
	m.tokens = append(m.tokens, t)
	return sealed, nil
}

// taskRequest 是“完成”固定在令牌上的请求（所见任务版本）。
type taskRequest struct {
	Expected int64 `json:"expected_version"`
}

func pursuitCode(err error) (string, bool) {
	var pe *pursuitapi.Error
	if errors.As(err, &pe) {
		return pe.Code, true
	}
	return "", false
}

// executeTask 执行一次任务提醒卡片上的点击（见文件头）。
func (s *Service) executeTask(sctx, uctx context.Context, j actionJob, tok *actionToken) string {
	if s.Pursuits == nil {
		s.transient(sctx, j, tok)
		return "transient"
	}
	view, err := s.Pursuits.GetTask(uctx, tok.task)
	if err != nil {
		if dependencyTransient(err) {
			s.transient(sctx, j, tok)
			return "transient"
		}
		res := &actionResult{Result: "failed", Code: "forbidden"}
		s.settleTask(sctx, uctx, j, tok, res, nil)
		return res.Result
	}
	var req *taskRequest
	if tok.action == "complete_task" {
		switch {
		case view.Task.DeadlineVersion != tok.deadline:
			res := &actionResult{Result: "failed", Code: "deadline_changed"}
			s.settleTask(sctx, uctx, j, tok, res, view)
			return res.Result
		case !view.ViewerIsAssignee && !view.ViewerEscalated.Or(false):
			res := &actionResult{Result: "failed", Code: "forbidden"}
			s.settleTask(sctx, uctx, j, tok, res, view)
			return res.Result
		}
		req, err = s.pinTaskRequest(sctx, tok, view.Task.Version)
		if err != nil {
			s.transient(sctx, j, tok)
			return "transient"
		}
	}
	var fixed *actionResult
	for i := 0; fixed == nil && i < domain.MaxActionAttempts; i++ {
		n, err := s.incrementAttempt(sctx, tok.id)
		if err != nil {
			break
		}
		key := fmt.Sprintf("%s-%d", tok.id, n)
		var res *pursuitapi.CommandResult
		if tok.action == "complete_task" {
			res, err = s.Pursuits.CompleteTask(uctx, tok.task, key, &pursuitapi.CompleteTaskRequest{OperationID: purapi.OperationID(tok.id),
				ExpectedVersion: req.Expected, Channel: purapi.NewOptChannel(purapi.ChannelWecom)})
		} else {
			res, err = s.Pursuits.ReopenTask(uctx, tok.task, key, &pursuitapi.ReopenTaskRequest{OperationID: purapi.OperationID(tok.id),
				TargetOperationID: purapi.OperationID(tok.targetOp), Channel: purapi.NewOptChannel(purapi.ChannelWecom)})
		}
		switch {
		case err == nil:
			fixed = &actionResult{Result: "succeeded"}
			if res.Result == purapi.CommandResultResultNoChange {
				fixed.Result = "no_change"
			}
			if p, ok := res.Pursuit.Get(); ok {
				fixed.PursuitID = string(p.ID)
			}
		case !dependencyTransient(err):
			code, _ := pursuitCode(err)
			if code == "" {
				code = "failed"
			}
			if code == "authorization_syncing" {
				// 不固定（pursuit 也不固定在操作上）：授权同步完成后同一令牌可再执行
				s.taskNotSettled(sctx, j, tok, code)
				return code
			}
			fixed = &actionResult{Result: "failed", Code: code}
		}
	}
	if fixed == nil {
		s.transient(sctx, j, tok)
		return "transient"
	}
	if fixed.PursuitID == "" {
		fixed.PursuitID = string(view.Task.PursuitID)
	}
	s.settleTask(sctx, uctx, j, tok, fixed, view)
	return fixed.Result
}

// pinTaskRequest 返回“完成”的请求：首次执行时以所见任务版本构造并固定在令牌上，之后的每次尝试发送同一请求。
func (s *Service) pinTaskRequest(sctx context.Context, tok *actionToken, version int64) (*taskRequest, error) {
	if tok.request == nil {
		raw, err := json.Marshal(taskRequest{Expected: version})
		if err != nil {
			return nil, err
		}
		err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE chn_action_tokens SET request = $2 WHERE id = $1 AND request IS NULL`, tok.id, raw); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT request FROM chn_action_tokens WHERE id = $1`, tok.id).Scan(&tok.request)
		})
		if err != nil {
			return nil, err
		}
	}
	var req taskRequest
	return &req, json.Unmarshal(tok.request, &req)
}

// taskNotSettled 回复“授权同步中”：令牌不作废、不固定结果，只写审计。
func (s *Service) taskNotSettled(sctx context.Context, j actionJob, tok *actionToken, code string) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		return recordAs(ctx, tx, "channels.action.deferred", tok.id, "failed", "wecom;operation="+tok.id+";code="+code)
	})
	if err != nil {
		s.logger().WarnContext(sctx, "动作审计写入失败", "token", tok.id, "error", err.Error())
	}
	s.reply(sctx, j, nil, "授权同步中，稍后可再点一次或打开 Web 处理", s.viewButton(sctx, tok))
}

// settleTask 把确定结果固定在令牌上（只一次），发布 channels.task_action.executed.v1 并写审计，然后回写卡片。
func (s *Service) settleTask(sctx, uctx context.Context, j actionJob, tok *actionToken, res *actionResult, view *pursuitapi.TaskView) {
	if res.PursuitID == "" && view != nil {
		res.PursuitID = string(view.Task.PursuitID)
	}
	raw, _ := json.Marshal(res)
	now := s.businessNow()
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE chn_action_tokens SET state = 'settled', result = $2, executed_at = $3 WHERE id = $1 AND state = 'open'`, tok.id, raw, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return tx.QueryRow(ctx, `SELECT result, executed_at FROM chn_action_tokens WHERE id = $1`, tok.id).Scan(&raw, &now)
		}
		data := map[string]any{"operation_id": tok.id, "recipient": tok.recipient, "task_id": tok.task, "action": tok.action, "result": res.Result,
			"channel": Channel, "executed_at": now.UTC().Format(time.RFC3339Nano)}
		if res.PursuitID != "" {
			data["pursuit_id"] = res.PursuitID
		}
		if tok.action == "reopen_task" {
			data["target_operation_id"] = tok.targetOp
		}
		if res.Result == "failed" {
			data["code"] = res.Code
		}
		if _, err := writer.Append(ctx, tx, outbox.Event{Type: TaskActionExecuted, Subject: tok.id, PartitionKey: tok.task, Data: data}); err != nil {
			return err
		}
		auditResult := "succeeded"
		if res.Result == "failed" {
			auditResult = "failed"
		}
		return recordAs(ctx, tx, "channels.action.executed", tok.id, auditResult, fmt.Sprintf("wecom;operation=%s;action=%s;task=%s;result=%s;code=%s", tok.id,
			tok.action, tok.task, res.Result, res.Code))
	})
	if err != nil {
		s.logger().WarnContext(sctx, "动作结果写入失败", "token", tok.id, "error", err.Error())
		return
	}
	_ = json.Unmarshal(raw, res)
	tok.state, tok.executedAt = "settled", &now
	s.replyTask(sctx, uctx, j, tok, res, false)
}

var taskStatusLabel = map[string]string{"todo": "待办", "done": "已完成", "cancelled": "已取消"}

// taskResultText 是任务提醒卡片动作结果的文字（F9 第3条）。
func taskResultText(tok *actionToken, res *actionResult, title string) string {
	named := func(prefix string) string {
		if title == "" {
			return prefix
		}
		return prefix + "：" + title
	}
	if tok.action == "reopen_task" {
		switch res.Result {
		case "succeeded":
			return named("已撤销完成，任务恢复为待办")
		case "no_change":
			return "无变化"
		}
		switch res.Code {
		case "undo_window_expired":
			return "已超过撤销时限（10 分钟），请在 Web 中处理"
		case "conflict":
			return "未撤销：任务之后已被改动，保持当前值"
		case "operation_not_found":
			return "没有可撤销的完成"
		case "forbidden", "task_not_found":
			return "你已没有该任务的操作权限"
		}
		return "操作未执行（" + res.Code + "）"
	}
	switch res.Result {
	case "succeeded":
		return named("已完成")
	case "no_change":
		return named("无变化，任务已完成")
	}
	switch res.Code {
	case "forbidden", "task_not_found":
		return "你已不是该任务的负责人，不能在卡片上完成"
	case "deadline_changed":
		return "任务截止时间已改变，请以新的提醒为准"
	case "conflict":
		return "未完成：任务已被改动，请查看当前任务后再操作"
	case "task_closed":
		return "任务已取消或跟进已关闭"
	}
	return "操作未执行（" + res.Code + "）"
}

// replyTask 回写任务提醒卡片：当前值以点击者本人经 pursuit 读取（已不能查看时不显示标题与状态）；成功的“完成”带“撤销”。
func (s *Service) replyTask(sctx, uctx context.Context, j actionJob, tok *actionToken, res *actionResult, replayed bool) {
	var lines []string
	title := ""
	if s.Pursuits != nil {
		if v, err := s.Pursuits.GetTask(uctx, tok.task); err == nil {
			title = v.Task.Title
			lines = append(lines, "当前："+taskStatusLabel[string(v.Task.Status)])
		}
	}
	text := taskResultText(tok, res, title)
	if replayed && tok.executedAt != nil {
		text += "（已于 " + clock(*tok.executedAt, tenantLocation(), nil, "01-02 15:04") + " 执行）"
	}
	buttons := s.viewButton(sctx, tok)
	if !replayed && res.Result == "succeeded" && tok.action == "complete_task" {
		m := &minter{s: s, tenant: tenantOf(sctx), recipient: tok.recipient, now: s.businessNow()}
		if undo, err := m.taskToken(tok.task, tok.objective, "reopen_task", tok.card, tok.id, tok.deadline, domain.UndoTokenTTL); err == nil {
			if err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
				return insertTokens(ctx, tx, m.tenant, "", m.tokens, m.now)
			}); err == nil {
				buttons = append([]api.Button{{Key: "undo", Label: "撤销", Token: undo}}, buttons...)
			}
		}
	}
	s.reply(sctx, j, lines, text, buttons)
}
