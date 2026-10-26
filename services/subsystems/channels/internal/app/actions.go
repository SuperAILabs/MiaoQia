package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	detapi "github.com/superailabs/miaoqia/services/contracts/apis/detection"
	purapi "github.com/superailabs/miaoqia/services/contracts/apis/pursuit"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	"github.com/superailabs/miaoqia/services/subsystems/channels/internal/domain"
	detection "github.com/superailabs/miaoqia/services/subsystems/detection/api"
	pursuitapi "github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// 卡片上的快捷动作（PRD 第5段 F4、F5、第9节）。回调先验签、核对时间窗，再校验动作令牌的签名（被篡改或伪造的令牌在任何读取之前拒绝）
// 与租户（令牌的租户须等于回调企业所属的租户，否则与不存在相同），按渠道事件标识去重后排队，1 秒内确认、异步执行。执行时按顺序检查：
// 点击者的有效绑定与成员资格、点击者就是令牌的收件人、已固定的结果、令牌未过期；之后由 detection 以点击者当前的权限判断。
// 操作开始前的拒绝只结束这一次点击并写审计，不消耗令牌、不固定结果；只有业务调用得到的确定结果才固定在令牌（= 操作标识）上。

// receiveAction 处理一次卡片按钮回调（验签与时间窗已通过）。
func (s *Service) receiveAction(ctx context.Context, r *corpRoute, ev *api.InboundEvent) (*api.InboundResult, error) {
	tctx := serviceContext(ctx, r.tenant, r.cell)
	var claims actionClaims
	reason := ""
	switch {
	case !s.unseal(purposeAction, ev.Action, &claims):
		reason = "token_invalid"
	case claims.Tenant != r.tenant:
		reason = "cross_tenant"
	case claims.Card != ev.CardID:
		reason = "card_mismatch"
	}
	if reason != "" {
		s.rejectCallback(ctx, ev.CorpID, "action_"+reason)
		err := s.tx(tctx, func(ctx context.Context, tx pgx.Tx) error {
			return recordAs(ctx, tx, "channels.action.rejected", "callback:"+ev.EventID, "denied", "wecom;"+reason)
		})
		if err != nil {
			return nil, err
		}
		return &api.InboundResult{Outcome: api.InboundRejected}, nil
	}
	out := &api.InboundResult{}
	err := s.tx(tctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO chn_callback_events (channel, corp_id, event_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, Channel, ev.CorpID, ev.EventID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			out.Outcome = api.InboundDuplicate
			return nil
		}
		var state string
		err = tx.QueryRow(ctx, `SELECT state FROM chn_installations WHERE id = $1 FOR SHARE`, r.installation).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "pending_verification" && state != "enabled") {
			out.Outcome = api.InboundIgnored
			return nil
		}
		if err != nil {
			return err
		}
		raw := make([]byte, 12)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO chn_action_jobs (id, tenant_id, token_id, corp_id, channel_user_id, card_id, event_id, state, received_at)
 VALUES ($1, $2, $3, $4, $5, $6, $7, 'queued', $8)`, "acj-"+hex.EncodeToString(raw), r.tenant, claims.ID, ev.CorpID, ev.UserID, ev.CardID, ev.EventID, s.now()); err != nil {
			return err
		}
		out.Outcome = api.InboundQueued
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.Outcome == api.InboundDuplicate {
		s.rejectCallback(ctx, ev.CorpID, "duplicate")
	}
	return out, nil
}

type actionToken struct {
	id, recipient, signal, objective, action, reason, target, card, state string
	content                                                               int64
	expires                                                               time.Time
	request, result                                                       []byte
	executedAt                                                            *time.Time
	// 任务提醒卡片（complete_task / reopen_task）：任务、提醒所在的截止版本、被撤销的“完成”
	task, targetOp string
	deadline       int64
}

func (t *actionToken) taskAction() bool {
	return t.action == "complete_task" || t.action == "reopen_task"
}

func (s *Service) readToken(sctx context.Context, id string) (*actionToken, error) {
	var t *actionToken
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		var v actionToken
		var reason, target, signal, task, targetOp *string
		var deadline *int64
		err := tx.QueryRow(ctx, `SELECT id, recipient, signal_id, objective_id, action, reason, target_token, card_id, state, content_version, expires_at, request, result, executed_at,
 task_id, deadline_version, target_operation FROM chn_action_tokens WHERE id = $1`, id).Scan(&v.id, &v.recipient, &signal, &v.objective, &v.action, &reason, &target, &v.card,
			&v.state, &v.content, &v.expires, &v.request, &v.result, &v.executedAt, &task, &deadline, &targetOp)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if reason != nil {
			v.reason = *reason
		}
		if target != nil {
			v.target = *target
		}
		if signal != nil {
			v.signal = *signal
		}
		if task != nil {
			v.task = *task
		}
		if deadline != nil {
			v.deadline = *deadline
		}
		if targetOp != nil {
			v.targetOp = *targetOp
		}
		t = &v
		return nil
	})
	return t, err
}

type actionJob struct {
	id, token, corp, user, card string
}

// ProcessActions 是 Briefings.ProcessActions。依赖不可用的点击留在队列中稍后再执行。
func (s *Service) ProcessActions(ctx context.Context, limit int) (int, error) {
	rc, err := requireService(ctx)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	sctx := serviceContext(ctx, rc.Tenant, rc.Cell)
	var jobs []actionJob
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, token_id, corp_id, channel_user_id, card_id FROM chn_action_jobs WHERE state = 'queued' ORDER BY received_at, id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j actionJob
			if err := rows.Scan(&j.id, &j.token, &j.corp, &j.user, &j.card); err != nil {
				return err
			}
			jobs = append(jobs, j)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	done := 0
	for _, j := range jobs {
		outcome, retry := s.processJob(sctx, rc, j)
		if retry {
			continue
		}
		err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE chn_action_jobs SET state = 'done', outcome = $2, done_at = $3 WHERE id = $1 AND state = 'queued'`, j.id, outcome, s.now())
			return err
		})
		if err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// reject 写一条操作开始前的拒绝审计（不触及令牌）。
func (s *Service) reject(sctx context.Context, token, reason string) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		return recordAs(ctx, tx, "channels.action.rejected", token, "denied", "wecom;"+reason)
	})
	if err != nil {
		s.logger().WarnContext(sctx, "动作拒绝审计写入失败", "token", token, "error", err.Error())
	}
}

// guide 为绑定无效的点击者发出绑定引导（不执行、不返回业务内容）。
func (s *Service) guide(sctx context.Context, j actionJob) {
	r, err := s.route(sctx, j.corp)
	if err != nil || r == nil {
		return
	}
	var token string
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		token, err = s.bindingRequest(ctx, tx, r, j.corp, j.user, "")
		return err
	})
	if err == nil {
		s.sendGuide(sctx, j.corp, j.user, token)
	}
}

func (s *Service) processJob(sctx context.Context, rc reqctx.Context, j actionJob) (string, bool) {
	tok, err := s.readToken(sctx, j.token)
	if err != nil {
		return "", true
	}
	if tok == nil || tok.card != j.card {
		s.reject(sctx, j.token, "token_unknown")
		return "rejected", false
	}
	// 点击者的渠道账号须有有效绑定，绑定的成员仍是租户有效成员（且绑定不来自退出之前）
	b, err := s.boundAccount(sctx, j.corp, j.user)
	if err != nil {
		return "", true
	}
	if b == nil {
		s.reject(sctx, tok.id, "binding_inactive")
		s.guide(sctx, j)
		return "binding_inactive", false
	}
	current, err := s.bindingCurrent(sctx, b)
	if err != nil {
		return "", true
	}
	if !current {
		s.retireStaleBinding(sctx, rc.Tenant, rc.Cell, b)
		s.reject(sctx, tok.id, "member_left")
		s.guide(sctx, j)
		return "member_left", false
	}
	// 卡片被转给他人：拒绝，不返回该操作的结果，不消耗令牌
	if b.subject != tok.recipient {
		s.reject(sctx, tok.id, "not_recipient")
		s.reply(sctx, j, nil, "此卡片发给其他成员，不能由你操作", nil)
		return "not_recipient", false
	}
	uctx := userContext(sctx, rc.Tenant, rc.Cell, b.subject)
	if tok.state == "settled" {
		s.replay(sctx, uctx, j, tok)
		return "replayed", false
	}
	if !s.now().Before(tok.expires) {
		s.reject(sctx, tok.id, "expired")
		s.reply(sctx, j, nil, "卡片已过期，请在 Web 中处理", s.viewButton(sctx, tok))
		return "expired", false
	}
	return s.execute(sctx, uctx, j, tok), false
}

// actionResult 是固定在令牌上的结果（channels.action.executed.v1 与卡片回写的依据）。
type actionResult struct {
	Result   string          `json:"result"`
	Code     string          `json:"code,omitempty"`
	Feedback string          `json:"feedback,omitempty"`
	Status   string          `json:"status,omitempty"`
	After    string          `json:"status_after,omitempty"`
	Current  json.RawMessage `json:"current,omitempty"`
	// 卡片“跟进”（PRD 第6段 F1 第1条）：created / existing_linked / existing 的跟进与负责人（点击者不能查看已有跟进时只给负责人）
	Pursuit   string `json:"pursuit,omitempty"`
	PursuitID string `json:"pursuit_id,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

// incrementAttempt 为一次尝试分配序号（幂等键 = 令牌标识 + 序号；每次尝试一个新键）。
func (s *Service) incrementAttempt(sctx context.Context, id string) (int, error) {
	var n int
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE chn_action_tokens SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts`, id).Scan(&n)
	})
	return n, err
}

// triageRequest 返回本操作的请求：首次执行时按信号的当前版本构造并固定在令牌上，之后的每次尝试发送同一请求。
func (s *Service) triageRequest(sctx, uctx context.Context, tok *actionToken) (*detection.TriageRequest, *actionResult, error) {
	if tok.request != nil {
		var req detection.TriageRequest
		if err := json.Unmarshal(tok.request, &req); err != nil {
			return nil, nil, err
		}
		return &req, nil, nil
	}
	cards, err := s.Detection.SignalCards(uctx, []string{tok.signal})
	if err != nil {
		return nil, nil, err
	}
	if len(cards.Cards) == 0 {
		// 点击者已不能查看该信号：确定失败（FORBIDDEN）
		return nil, &actionResult{Result: "failed", Code: "forbidden"}, nil
	}
	c := cards.Cards[0].Signal
	req := detection.TriageRequest{OperationID: tok.id, Action: detapi.TriageRequestActionFeedback, SeenContentVersion: tok.content,
		StateVersion: c.StateVersion, FeedbackVersion: c.FeedbackVersion, Channel: detapi.NewOptTriageRequestChannel(detapi.TriageRequestChannelWecom)}
	if tok.action == "relevant" {
		req.Feedback = detapi.NewOptFeedbackInput(detapi.FeedbackInput{Result: detapi.FeedbackInputResultRelevant, Reasons: []string{}})
	} else {
		req.Feedback = detapi.NewOptFeedbackInput(detapi.FeedbackInput{Result: detapi.FeedbackInputResultIrrelevant, Reasons: []string{tok.reason}})
		req.Dismiss = detapi.NewOptBool(true)
	}
	raw, err := json.Marshal(&req)
	if err != nil {
		return nil, nil, err
	}
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE chn_action_tokens SET request = $2 WHERE id = $1 AND request IS NULL`, tok.id, raw); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT request FROM chn_action_tokens WHERE id = $1`, tok.id).Scan(&tok.request)
	})
	if err != nil {
		return nil, nil, err
	}
	var fixed detection.TriageRequest
	return &fixed, nil, json.Unmarshal(tok.request, &fixed)
}

func partChanges(p detapi.TriageParts) (feedback, status, after string) {
	if f, ok := p.Feedback.Get(); ok {
		feedback = string(f.Change)
	}
	if st, ok := p.Status.Get(); ok {
		status = string(st.Change)
		if a, ok := st.After.Get(); ok {
			after = string(a)
		}
	}
	return
}

func definiteOf(err error) (string, json.RawMessage) {
	var de *detection.Error
	if errors.As(err, &de) {
		return de.Code, de.Current
	}
	return "failed", nil
}

// execute 执行一次点击：至多 MaxActionAttempts 次尝试（每次新的幂等键、同一操作标识与请求）；成功、无变化与确定失败固定在令牌上，
// 暂时失败不作废令牌。
func (s *Service) execute(sctx, uctx context.Context, j actionJob, tok *actionToken) string {
	if tok.taskAction() {
		return s.executeTask(sctx, uctx, j, tok)
	}
	if s.Detection == nil {
		s.reply(sctx, j, nil, "暂时没有成功，可再点一次或打开 Web 处理", s.viewButton(sctx, tok))
		return "transient"
	}
	if tok.action == "pursue" {
		return s.executePursue(sctx, uctx, j, tok)
	}
	var fixed *actionResult
	var triage *detection.TriageRequest
	if tok.action != "undo" {
		var err error
		triage, fixed, err = s.triageRequest(sctx, uctx, tok)
		if err != nil {
			s.transient(sctx, j, tok)
			return "transient"
		}
	}
	for i := 0; fixed == nil && i < domain.MaxActionAttempts; i++ {
		n, err := s.incrementAttempt(sctx, tok.id)
		if err != nil {
			break
		}
		key := fmt.Sprintf("%s-%d", tok.id, n)
		if tok.action == "undo" {
			res, err := s.Detection.Undo(uctx, key, &detection.UndoRequest{OperationID: tok.id, TargetOperationID: detapi.NewOptString(tok.target),
				Channel: detapi.NewOptUndoRequestChannel(detapi.UndoRequestChannelWecom)})
			switch {
			case err == nil && len(res.Items) == 0:
				fixed = &actionResult{Result: "failed", Code: "not_applicable"}
			case err == nil:
				it := res.Items[0]
				if it.Result == detapi.UndoItemResultSucceeded {
					fixed = &actionResult{Result: "succeeded"}
					if p, ok := it.Parts.Get(); ok {
						fixed.Feedback, fixed.Status, fixed.After = partChanges(p)
					}
				} else if it.Failure.Or("") != detapi.UndoItemFailureTransient {
					fixed = &actionResult{Result: "failed", Code: it.Code.Or("failed")}
					if c, ok := it.Current.Get(); ok {
						fixed.Current, _ = c.MarshalJSON()
					}
				}
			case !dependencyTransient(err):
				code, cur := definiteOf(err)
				fixed = &actionResult{Result: "failed", Code: code, Current: cur}
			}
			continue
		}
		res, err := s.Detection.Triage(uctx, tok.signal, key, triage)
		switch {
		case err == nil:
			fixed = &actionResult{Result: string(res.Result)}
			fixed.Feedback, fixed.Status, fixed.After = partChanges(res.Parts)
		case !dependencyTransient(err):
			code, cur := definiteOf(err)
			fixed = &actionResult{Result: "failed", Code: code, Current: cur}
		}
	}
	if fixed == nil {
		s.transient(sctx, j, tok)
		return "transient"
	}
	s.settle(sctx, uctx, j, tok, fixed)
	return fixed.Result
}

// executePursue 执行卡片上的“跟进”：以点击者的用户主体调用 pursuit 的渠道内创建（负责人为点击者本人、没有任务），授权由 pursuit
// 按点击者此刻在目标上的 can_assign 判断（channels 不另作判断）。操作标识为令牌标识，每次尝试一个新的幂等键；pursuit 的确定失败
// （4xx，例如 forbidden、signal_updated、signal_closed）固定在令牌上，暂时失败不作废令牌。
func (s *Service) executePursue(sctx, uctx context.Context, j actionJob, tok *actionToken) string {
	if s.Pursuits == nil {
		s.transient(sctx, j, tok)
		return "transient"
	}
	req := &pursuitapi.CreatePursuitRequest{OperationID: purapi.OperationID(tok.id), ObjectiveID: purapi.ObjectiveID(tok.objective),
		SignalID: purapi.NewOptSignalID(purapi.SignalID(tok.signal)), SeenContentVersion: purapi.NewOptInt64(tok.content),
		Collaborators: []purapi.Subject{}, Channel: purapi.NewOptChannel(purapi.ChannelWecom)}
	var fixed *actionResult
	for i := 0; fixed == nil && i < domain.MaxActionAttempts; i++ {
		n, err := s.incrementAttempt(sctx, tok.id)
		if err != nil {
			break
		}
		res, err := s.Pursuits.CreatePursuit(uctx, fmt.Sprintf("%s-%d", tok.id, n), req)
		switch {
		case err == nil:
			fixed = &actionResult{Result: "succeeded", Pursuit: string(res.Result)}
			if res.Result == purapi.CommandResultResultExisting {
				fixed.Result = "no_change"
			}
			if p, ok := res.Pursuit.Get(); ok {
				fixed.PursuitID, fixed.Owner = string(p.ID), string(p.Owner)
			} else if o, ok := res.ExistingOwner.Get(); ok {
				fixed.Owner = string(o)
			}
		case !dependencyTransient(err):
			fixed = &actionResult{Result: "failed", Code: "failed"}
			var pe *pursuitapi.Error
			if errors.As(err, &pe) {
				fixed.Code = pe.Code
			}
		}
	}
	if fixed == nil {
		s.transient(sctx, j, tok)
		return "transient"
	}
	s.settle(sctx, uctx, j, tok, fixed)
	return fixed.Result
}

func (s *Service) transient(sctx context.Context, j actionJob, tok *actionToken) {
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		return recordAs(ctx, tx, "channels.action.transient", tok.id, "failed", "wecom;operation="+tok.id)
	})
	if err != nil {
		s.logger().WarnContext(sctx, "动作审计写入失败", "token", tok.id, "error", err.Error())
	}
	s.reply(sctx, j, nil, "暂时没有成功，可再点一次或打开 Web 处理", s.viewButton(sctx, tok))
}

// settle 把确定结果固定在令牌上（只一次），发布 channels.action.executed.v1 并写审计，然后回写卡片。
func (s *Service) settle(sctx, uctx context.Context, j actionJob, tok *actionToken, res *actionResult) {
	raw, _ := json.Marshal(res)
	now := s.businessNow()
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE chn_action_tokens SET state = 'settled', result = $2, executed_at = $3 WHERE id = $1 AND state = 'open'`, tok.id, raw, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// 并发的另一次执行已固定结果：以它为准
			return tx.QueryRow(ctx, `SELECT result, executed_at FROM chn_action_tokens WHERE id = $1`, tok.id).Scan(&raw, &now)
		}
		data := map[string]any{"operation_id": tok.id, "token_id": tok.id, "recipient": tok.recipient, "signal_id": tok.signal, "action": tok.action,
			"result": res.Result, "channel": Channel, "executed_at": now.UTC().Format(time.RFC3339Nano)}
		parts := map[string]any{}
		if res.Feedback != "" {
			parts["feedback"] = res.Feedback
		}
		if res.Status != "" {
			parts["status"] = res.Status
		}
		if len(parts) > 0 {
			data["parts"] = parts
		}
		if res.Result == "failed" {
			data["code"] = res.Code
		}
		if res.PursuitID != "" {
			data["pursuit_id"] = res.PursuitID
		}
		if _, err := writer.Append(ctx, tx, outbox.Event{Type: ActionExecuted, Subject: tok.id, PartitionKey: tok.signal, Data: data}); err != nil {
			return err
		}
		auditResult := "succeeded"
		if res.Result == "failed" {
			auditResult = "failed"
		}
		return recordAs(ctx, tx, "channels.action.executed", tok.id, auditResult, fmt.Sprintf("wecom;operation=%s;action=%s;result=%s;code=%s", tok.id, tok.action, res.Result, res.Code))
	})
	if err != nil {
		s.logger().WarnContext(sctx, "动作结果写入失败", "token", tok.id, "error", err.Error())
		return
	}
	_ = json.Unmarshal(raw, res)
	tok.state, tok.executedAt = "settled", &now
	s.replyResult(sctx, uctx, j, tok, res, false)
}

// replay 返回已固定的结果并注明执行时间，同时显示当前值（F5 第6条）；不重新执行。
func (s *Service) replay(sctx, uctx context.Context, j actionJob, tok *actionToken) {
	var res actionResult
	_ = json.Unmarshal(tok.result, &res)
	err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		return recordAs(ctx, tx, "channels.action.replayed", tok.id, "succeeded", "wecom;operation="+tok.id+";result="+res.Result)
	})
	if err != nil {
		s.logger().WarnContext(sctx, "动作审计写入失败", "token", tok.id, "error", err.Error())
	}
	if tok.taskAction() {
		s.replyTask(sctx, uctx, j, tok, &res, true)
		return
	}
	s.replyResult(sctx, uctx, j, tok, &res, true)
}

var statusLabel = map[string]string{"open": "待处理", "triaged": "待处理（已调整处置）", "dismissed": "已忽略", "converted": "已转跟进", "closed": "已关闭"}

// resultText 是卡片动作结果的文字（按部分，F4 第7条）。
func (s *Service) resultText(tok *actionToken, res *actionResult, current, objectiveCode string) string {
	// 原因按令牌所属目标的反馈标签显示（不同目标的同一代码可能措辞不同）
	say := func(code string) string {
		for _, r := range s.Packs.Quick[objectiveCode] {
			if r.Code == code {
				return r.Say
			}
		}
		return code
	}
	if tok.action == "pursue" {
		return pursueText(res)
	}
	switch res.Result {
	case "failed":
		switch res.Code {
		case "forbidden", "signal_not_found":
			return "你已没有该目标的反馈权限"
		case "signal_updated":
			return "这条信号已更新，请查看新卡片后再操作"
		case "signal_closed":
			return "该信号已关闭"
		case "conflict":
			return "未撤销：信号之后已被修改，反馈与状态保持当前值"
		case "triage_operation_not_found":
			return "没有可撤销的操作"
		}
		return "操作未执行（" + res.Code + "）"
	case "no_change":
		return "无变化"
	}
	if tok.action == "undo" {
		return "已撤销"
	}
	text := "已标为相关"
	if tok.action == "irrelevant" {
		text = "已标为不相关"
		if tok.reason != "" {
			text += "（" + say(tok.reason) + "）"
		}
		switch res.Status {
		case "changed":
			text += "，已移出待处理"
		case "unchanged":
			text = "已记录你的反馈；状态无变化"
		case "not_applicable":
			after := res.After
			if after == "" {
				after = current
			}
			text = "已记录你的反馈；该信号" + statusLabel[after] + "，处理状态不适用"
		}
		if res.Feedback == "unchanged" && res.Status == "changed" {
			text = "反馈无变化；已移出待处理"
		}
	}
	return text
}

// pursueText 是卡片“跟进”的结果文字（PRD 第6段 F1 第1、4条）。
func pursueText(res *actionResult) string {
	switch res.Result {
	case "failed":
		switch res.Code {
		case "forbidden", "objective_not_found", "signal_not_found":
			return "你没有在该目标上创建跟进的权限"
		case "signal_updated":
			return "这条信号已更新，请查看新卡片后再跟进"
		case "signal_closed":
			return "该信号已关闭，不能创建跟进"
		case "not_applicable":
			return "该信号不能创建跟进"
		}
		return "跟进未创建（" + res.Code + "）"
	}
	switch res.Pursuit {
	case "existing_linked":
		return "该对象已有进行中的跟进（负责人 " + res.Owner + "），已把这条信号关联到它"
	case "existing":
		if res.Owner != "" {
			return "该对象已有进行中的跟进（负责人 " + res.Owner + "）；无变化"
		}
		return "该对象已有进行中的跟进；无变化"
	}
	return "已创建跟进，负责人为你；可在 Web 中添加任务与截止时间"
}

// replyResult 回写卡片：渠道支持时只更新点击者本人的这张卡片，否则（或更新失败时）另发一条结果消息。成功的反馈带“撤销”（10 分钟）；
// SIGNAL_UPDATED 另发一张按新版本渲染、带新令牌的卡片。
func (s *Service) replyResult(sctx, uctx context.Context, j actionJob, tok *actionToken, res *actionResult, replayed bool) {
	// 当前值（状态叠加，“当前”）：同事已处理、之后已撤销等
	var lines []string
	current := ""
	if cards, err := s.Detection.SignalCards(uctx, []string{tok.signal}); err == nil && len(cards.Cards) > 0 {
		c := cards.Cards[0]
		current = string(c.Signal.Status)
		cur := "当前：" + statusLabel[current]
		if ch, ok := c.LastStatusChange.Get(); ok && ch.Actor != tok.recipient && (c.Signal.Status == "dismissed" || c.Signal.Status == "converted") {
			cur += "（" + ch.Actor + " 于 " + clock(ch.At, tenantLocation(), nil, "15:04") + "）"
		}
		lines = append(lines, cur)
	}
	code := ""
	if tok.action == "irrelevant" {
		code, _ = s.objectiveCode(sctx, tok.objective)
	}
	text := s.resultText(tok, res, current, code)
	if replayed && tok.executedAt != nil {
		text += "（已于 " + clock(*tok.executedAt, tenantLocation(), nil, "01-02 15:04") + " 执行）"
	}
	buttons := s.viewButton(sctx, tok)
	// “跟进”没有渠道内撤销（撤销创建在 Web 的跟进详情中，10 分钟内由创建者执行）
	if !replayed && res.Result == "succeeded" && tok.action != "undo" && tok.action != "pursue" {
		m := &minter{s: s, tenant: tenantOf(sctx), recipient: tok.recipient, now: s.businessNow()}
		if undo, err := m.token(tok.signal, tok.objective, "undo", "", tok.card, tok.id, 0, domain.UndoTokenTTL); err == nil {
			if err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
				return insertTokens(ctx, tx, m.tenant, "", m.tokens, m.now)
			}); err == nil {
				buttons = append([]api.Button{{Key: "undo", Label: "撤销", Token: undo}}, buttons...)
			}
		}
	}
	s.reply(sctx, j, lines, text, buttons)
	if !replayed && res.Code == "signal_updated" {
		s.freshCard(sctx, uctx, j, tok)
	}
}

func tenantOf(ctx context.Context) string {
	rc, _ := reqctx.From(ctx)
	return rc.Tenant
}

// viewButton 返回“查看”链接按钮（新的只指向信号的链接）。
func (s *Service) viewButton(sctx context.Context, tok *actionToken) []api.Button {
	if tok.taskAction() {
		return []api.Button{{Key: "view", Label: "查看", URL: taskURL(tok.task)}}
	}
	m := &minter{s: s, tenant: tenantOf(sctx), recipient: tok.recipient, now: s.businessNow()}
	url, err := m.link("signal", tok.signal)
	if err != nil {
		return nil
	}
	if err := s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error { return insertLinks(ctx, tx, m.tenant, m.links, m.now) }); err != nil {
		return nil
	}
	return []api.Button{{Key: "view", Label: "查看", URL: url}}
}

// reply 把结果写回点击的卡片（UpdateCard），不支持或失败时另发一条结果消息。
func (s *Service) reply(sctx context.Context, j actionJob, lines []string, text string, buttons []api.Button) {
	msg := api.Message{Kind: "action_result", Text: text, Card: &api.Card{CardID: j.card, Title: text, Lines: lines, Buttons: buttons}}
	if s.provider().Capabilities().UpdateCards {
		if err := s.provider().UpdateCard(sctx, j.corp, j.user, j.card, msg); err == nil {
			return
		}
	}
	msg.Card = nil
	if len(buttons) > 0 {
		msg.URL = buttons[len(buttons)-1].URL
	}
	if err := s.provider().Send(sctx, j.corp, j.user, msg); err != nil {
		s.logger().WarnContext(sctx, "动作结果发送失败", "card", j.card, "error", err.Error())
	}
}

// freshCard 按信号的当前版本渲染一张带新令牌的卡片（F4 第10条）。
func (s *Service) freshCard(sctx, uctx context.Context, j actionJob, tok *actionToken) {
	cards, err := s.Detection.SignalCards(uctx, []string{tok.signal})
	if err != nil || len(cards.Cards) == 0 {
		return
	}
	c := cards.Cards[0]
	rights := s.fieldRights(sctx, []string{tok.signal})
	it := signalFields(detection.BriefingSignal{Signal: c.Signal, Category: detapi.BriefingSignalCategoryUpdated, Updates: []detapi.SignalUpdate{}}, rights[tok.signal])
	var prefs = s.defaults()
	_ = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		sched, err := s.schedule(ctx, tx, tenantOf(sctx), tok.recipient)
		if err == nil {
			prefs = sched[len(sched)-1]
		}
		return err
	})
	code, title := "", ""
	if pol, err := s.objectiveCode(sctx, tok.objective); err == nil {
		code, title = pol, pol
		if sec, ok := s.Packs.Sections[pol]; ok {
			title = sec.Title
		}
	}
	m := &minter{s: s, tenant: tenantOf(sctx), recipient: tok.recipient, now: s.businessNow()}
	m.pursue = s.pursueGrants(sctx, m.tenant, tok.recipient, []string{tok.objective})
	msg, err := m.signalCard(code, title, tok.objective, it, c.ViewerCanFeedback, prefs)
	if err != nil {
		return
	}
	msg.Text = fmt.Sprintf("这条信号已更新（内容版本 %d → %d），以下是新卡片", tok.content, c.Signal.ContentVersion)
	err = s.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertTokens(ctx, tx, m.tenant, "", m.tokens, m.now); err != nil {
			return err
		}
		return insertLinks(ctx, tx, m.tenant, m.links, m.now)
	})
	if err == nil {
		if err := s.provider().Send(sctx, j.corp, j.user, msg); err != nil {
			s.logger().WarnContext(sctx, "新卡片发送失败", "error", err.Error())
		}
	}
}

// objectiveCode 返回目标的编号（读取生效策略）。
func (s *Service) objectiveCode(sctx context.Context, objective string) (string, error) {
	pages, err := s.activeObjectives(sctx)
	if err != nil {
		return "", err
	}
	for _, p := range pages {
		for _, o := range p.Policies {
			if o.ObjectiveID == objective {
				return o.ObjectiveCode, nil
			}
		}
	}
	return "", errors.New("objective not found")
}
