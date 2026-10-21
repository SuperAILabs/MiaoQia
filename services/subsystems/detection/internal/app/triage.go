package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 处理与反馈（PRD 第3段 F4、F5、F9；detection.md 第12节）。一个动作最多改变三个部分：本人反馈、处理状态、人工处置。
// 操作与尝试分开：操作标识（operation_id）固定一次用户意图，提交后的结果（成功、无变化、确定失败）固定在操作标识上；
// 幂等键是一次尝试，同键重发返回该尝试的结果（含失败）。暂时失败（503，未提交）只记在尝试上。

// FeedbackInput 是请求中的反馈（契约 FeedbackInput）。
type FeedbackInput struct {
	Result  string   `json:"result"`
	Reasons []string `json:"reasons,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// Request 是一次处理动作（契约 TriageRequest；批量中每项由批次动作转换而来）。
type Request struct {
	OperationID     string         `json:"operation_id"`
	Action          string         `json:"action"`
	SeenContent     int64          `json:"seen_content_version"`
	StateVersion    int64          `json:"state_version"`
	FeedbackVersion int64          `json:"feedback_version"`
	Feedback        *FeedbackInput `json:"feedback,omitempty"`
	Dismiss         bool           `json:"dismiss,omitempty"`
	Manual          string         `json:"manual_disposition,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	Channel         string         `json:"channel,omitempty"`
}

// FeedbackPart、StatusPart 与 ManualPart 是动作的各部分（契约 TriageParts）。
type FeedbackPart struct {
	Change  string    `json:"change"`
	Before  *Feedback `json:"before,omitempty"`
	After   *Feedback `json:"after,omitempty"`
	Version int64     `json:"feedback_version,omitempty"`
}

type StatusPart struct {
	Change  string `json:"change"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
	Version int64  `json:"state_version,omitempty"`
}

type ManualPart struct {
	Change  string `json:"change"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
	Version int64  `json:"state_version,omitempty"`
}

type Parts struct {
	Feedback *FeedbackPart `json:"feedback,omitempty"`
	Status   *StatusPart   `json:"status,omitempty"`
	Manual   *ManualPart   `json:"manual_disposition,omitempty"`
}

func (p *Parts) changed() bool {
	return (p.Feedback != nil && p.Feedback.Change == "changed") || (p.Status != nil && p.Status.Change == "changed") ||
		(p.Manual != nil && p.Manual.Change == "changed")
}

// State 是信号的当前处理状态与本人反馈（契约 SignalState）。
type State struct {
	Status          string    `json:"status"`
	Manual          string    `json:"manual_disposition"`
	StateVersion    int64     `json:"state_version"`
	ContentVersion  int64     `json:"content_version"`
	Feedback        *Feedback `json:"feedback,omitempty"`
	FeedbackVersion int64     `json:"feedback_version"`
}

// Result 是一次动作的结果（契约 TriageResult）。
type Result struct {
	OperationID string `json:"operation_id"`
	SignalID    string `json:"signal_id"`
	Action      string `json:"action"`
	Result      string `json:"result"`
	Parts       Parts  `json:"parts"`
	State       State  `json:"state"`
}

// current 是动作执行时信号与本人反馈的当前值（已加行锁）。
type current struct {
	id, objective, disposition, status, manual, manualReason string
	stateVersion, contentVersion                             int64
	createdAt                                                time.Time
	fb                                                       *Feedback
	fbVersion                                                int64
}

func (c *current) state() State {
	m := c.manual
	if m == "" {
		m = "none"
	}
	return State{Status: c.status, Manual: m, StateVersion: c.stateVersion, ContentVersion: c.contentVersion, Feedback: c.fb, FeedbackVersion: c.fbVersion}
}

func stateJSON(s State) []byte {
	raw, _ := json.Marshal(s)
	return raw
}

func conflict(code string, c *current) *api.Error {
	return &api.Error{Status: 409, Code: code, Current: stateJSON(c.state())}
}

func definite(status int, code string) *api.Error { return &api.Error{Status: status, Code: code} }

// plan 是一次动作要写入的变化。
type plan struct {
	parts       Parts
	status      string
	manual      string
	reason      *string
	stateChange bool
	fb          *Feedback
	fbChange    bool
}

func manualValue(m string) string {
	if m == "" {
		return "none"
	}
	return m
}

// validateFeedback 校验反馈结果与原因标签（取自应用领域包）：不相关与排除有误至少一个原因，标为须说明的原因（“其他”）须写说明。
func validateFeedback(in *FeedbackInput, excluded bool, reasons *FeedbackReasons) *api.Error {
	if in == nil {
		return definite(422, "invalid_feedback")
	}
	switch in.Result {
	case "relevant", "irrelevant":
		if excluded {
			return definite(422, "not_applicable")
		}
	case "exclusion_correct", "exclusion_wrong":
		if !excluded {
			return definite(422, "not_applicable")
		}
	default:
		return definite(422, "invalid_feedback")
	}
	if (in.Result == "irrelevant" || in.Result == "exclusion_wrong") && len(in.Reasons) == 0 {
		return definite(422, "invalid_feedback")
	}
	var allowed []Reason
	if reasons != nil {
		allowed = reasons.of(in.Result)
	}
	for _, code := range in.Reasons {
		i := slices.IndexFunc(allowed, func(r Reason) bool { return r.Code == code })
		if i < 0 {
			return definite(422, "invalid_feedback")
		}
		if allowed[i].NoteRequired && in.Note == "" {
			return definite(422, "invalid_feedback")
		}
	}
	return nil
}

// decide 按部分判断一个动作（detection.md 第12节）：请求的效果已成立的部分为无变化，不写记录、不校验版本；其余部分须所带版本
// 等于当前版本，否则整个动作 409 conflict。已关闭信号的处理状态部分不适用（只含处理状态的动作 409 signal_closed）；已转跟进的
// 信号同样不适用（只含处理状态的动作 422 not_applicable）；排除信号只接受排除反馈。
func decide(c *current, req Request, reasons *FeedbackReasons) (*plan, *api.Error) {
	excluded := c.disposition == "excluded"
	p := &plan{status: c.status, manual: c.manual, fb: c.fb}
	statusOnly := false
	switch req.Action {
	case "feedback":
		if err := validateFeedback(req.Feedback, excluded, reasons); err != nil {
			return nil, err
		}
		if req.Dismiss && excluded {
			return nil, definite(422, "not_applicable")
		}
	case "withdraw_feedback":
	case "dismiss", "restore":
		statusOnly = true
	case "adjust":
		statusOnly = true
		if req.Manual == "" || req.Reason == "" {
			return nil, definite(422, "invalid_feedback")
		}
	default:
		return nil, definite(400, "invalid_request")
	}
	if statusOnly {
		if excluded {
			return nil, definite(422, "not_applicable")
		}
		if c.status == "closed" {
			return nil, conflict("signal_closed", c)
		}
		if c.status == "converted" {
			return nil, definite(422, "not_applicable")
		}
	}
	if req.SeenContent != c.contentVersion {
		return nil, conflict("signal_updated", c)
	}
	needState, needFeedback := false, false
	switch req.Action {
	case "feedback", "withdraw_feedback":
		var next *Feedback
		if req.Action == "feedback" {
			rs := slices.Clone(req.Feedback.Reasons)
			if rs == nil {
				rs = []string{}
			}
			slices.Sort(rs)
			next = &Feedback{Result: req.Feedback.Result, Reasons: rs, Note: req.Feedback.Note, BasedOn: c.contentVersion}
		}
		if c.fb.equal(next) {
			p.parts.Feedback = &FeedbackPart{Change: "unchanged"}
		} else {
			needFeedback = true
			p.fb, p.fbChange = next, true
			p.parts.Feedback = &FeedbackPart{Change: "changed", Before: c.fb, After: next, Version: c.fbVersion + 1}
		}
		if req.Action == "feedback" && req.Dismiss {
			switch c.status {
			case "closed", "converted":
				p.parts.Status = &StatusPart{Change: "not_applicable"}
			case "dismissed":
				p.parts.Status = &StatusPart{Change: "unchanged"}
			default:
				needState = true
				p.status = "dismissed"
				p.parts.Status = &StatusPart{Change: "changed", Before: c.status, After: "dismissed"}
			}
		}
	case "dismiss":
		if c.status == "dismissed" {
			p.parts.Status = &StatusPart{Change: "unchanged"}
		} else {
			needState = true
			p.status = "dismissed"
			p.parts.Status = &StatusPart{Change: "changed", Before: c.status, After: "dismissed"}
		}
	case "restore":
		if c.status != "dismissed" {
			p.parts.Status = &StatusPart{Change: "unchanged"}
		} else {
			needState = true
			p.status = "open"
			if c.manual != "" {
				p.status = "triaged"
			}
			p.parts.Status = &StatusPart{Change: "changed", Before: c.status, After: p.status}
		}
	case "adjust":
		if c.manual == req.Manual {
			p.parts.Manual = &ManualPart{Change: "unchanged"}
		} else {
			needState = true
			p.manual = req.Manual
			p.parts.Manual = &ManualPart{Change: "changed", Before: manualValue(c.manual), After: req.Manual}
		}
		if c.status == "triaged" {
			p.parts.Status = &StatusPart{Change: "unchanged"}
		} else {
			needState = true
			p.status = "triaged"
			p.parts.Status = &StatusPart{Change: "changed", Before: c.status, After: "triaged"}
		}
		if needState {
			reason := req.Reason
			p.reason = &reason
		}
	}
	if (needState && req.StateVersion != c.stateVersion) || (needFeedback && req.FeedbackVersion != c.fbVersion) {
		return nil, conflict("conflict", c)
	}
	if needState {
		p.stateChange = true
		v := c.stateVersion + 1
		if p.parts.Status != nil && p.parts.Status.Change == "changed" {
			p.parts.Status.Version = v
		}
		if p.parts.Manual != nil && p.parts.Manual.Change == "changed" {
			p.parts.Manual.Version = v
		}
	}
	return p, nil
}

func fingerprint(parts ...any) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// stored 是固定在操作标识上的结果。
type stored struct {
	actor, fingerprint, outcome string
	status                      int
	response                    []byte
}

func readOp(ctx context.Context, tx pgx.Tx, id string) (*stored, error) {
	var o stored
	err := tx.QueryRow(ctx, `SELECT actor, fingerprint, outcome, status, response FROM det_triage_ops WHERE operation_id = $1`, id).
		Scan(&o.actor, &o.fingerprint, &o.outcome, &o.status, &o.response)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// replay 返回已固定的结果：他人或不同请求使用同一操作标识时 409 operation_conflict。
func replay[T any](o *stored, actor, fp string) (*T, error) {
	if o.actor != actor || o.fingerprint != fp {
		return nil, definite(409, "operation_conflict")
	}
	if o.outcome == "failed" {
		return nil, problemError(o.status, o.response)
	}
	var out T
	if err := json.Unmarshal(o.response, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// problem 是失败的固定响应（契约 Problem 的 code 与 current）。
type problem struct {
	Status  int             `json:"status"`
	Code    string          `json:"code"`
	Current json.RawMessage `json:"current,omitempty"`
}

func problemOf(e *api.Error) []byte {
	raw, _ := json.Marshal(problem{Status: e.Status, Code: e.Code, Current: e.Current})
	return raw
}

func problemError(status int, raw []byte) *api.Error {
	var p problem
	_ = json.Unmarshal(raw, &p)
	return &api.Error{Status: status, Code: p.Code, Current: p.Current}
}

// op 是要执行的一个操作（单条，或批量与撤销中的一项）。
type op struct {
	id, fingerprint, signal, objective, batch string
	req                                       Request
}

// lockSignal 读取信号与本人反馈并加行锁。
func lockSignal(ctx context.Context, tx pgx.Tx, signal, member string) (*current, error) {
	c := &current{id: signal}
	var manual, reason *string
	err := tx.QueryRow(ctx, `SELECT objective_id, disposition, status, manual_disposition, manual_reason, state_version, content_version, created_at
 FROM det_signals WHERE id = $1 FOR UPDATE`, signal).Scan(&c.objective, &c.disposition, &c.status, &manual, &reason, &c.stateVersion, &c.contentVersion, &c.createdAt)
	if err != nil {
		return nil, err
	}
	if manual != nil {
		c.manual = *manual
	}
	if reason != nil {
		c.manualReason = *reason
	}
	var result, note *string
	var reasons []string
	var based int64
	err = tx.QueryRow(ctx, `SELECT result, reasons, note, based_on_content_version, version FROM det_feedback WHERE signal_id = $1 AND member = $2 FOR UPDATE`,
		signal, member).Scan(&result, &reasons, &note, &based, &c.fbVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if result != nil {
		if reasons == nil {
			reasons = []string{}
		}
		c.fb = &Feedback{Result: *result, Reasons: reasons, BasedOn: based}
		if note != nil {
			c.fb.Note = *note
		}
	}
	return c, nil
}

// fixFailure 把确定失败固定在操作标识上（单独的事务）。并发的同一操作已固定结果时返回已固定的结果。
func fixFailure[T any](ctx context.Context, s *Service, rc reqctx.Context, o op, e *api.Error, undoOf *string) (*T, error) {
	var out *T
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "det-op/"+rc.Tenant+"/"+o.id); err != nil {
			return err
		}
		prev, err := readOp(ctx, tx, o.id)
		if err != nil {
			return err
		}
		if prev != nil {
			out, err = replay[T](prev, rc.Principal.ID, o.fingerprint)
			return err
		}
		return insertOp(ctx, tx, rc, o, "failed", e.Status, &e.Code, problemOf(e), nil, 0, undoOf, s.now())
	})
	if err != nil {
		return nil, err
	}
	if out != nil {
		return out, nil
	}
	return nil, e
}

func insertOp(ctx context.Context, tx pgx.Tx, rc reqctx.Context, o op, outcome string, status int, code *string, response, parts []byte,
	content int64, undoOf *string, at time.Time) error {
	if parts == nil {
		parts = []byte(`{}`)
	}
	action := o.req.Action
	if undoOf != nil {
		action = "undo"
	}
	channel := o.req.Channel
	if channel == "" {
		channel = "web"
	}
	var signal, objective, batch, reason *string
	if o.signal != "" {
		signal = &o.signal
	}
	if o.objective != "" {
		objective = &o.objective
	}
	if o.batch != "" {
		batch = &o.batch
	}
	if o.req.Reason != "" && action == "adjust" {
		reason = &o.req.Reason
	}
	_, err := tx.Exec(ctx, `INSERT INTO det_triage_ops (tenant_id, operation_id, actor, fingerprint, signal_id, objective_id, action, outcome, status, code,
 response, parts, reason, seen_content_version, content_version, channel, batch_id, undo_of, occurred_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		rc.Tenant, o.id, rc.Principal.ID, o.fingerprint, signal, objective, action, outcome, status, code, response, parts, reason,
		o.req.SeenContent, content, channel, batch, undoOf, at)
	return err
}

// eventFeedback 是事件中的反馈值（不含说明文字）。
func eventFeedback(f *Feedback) map[string]any {
	if f == nil {
		return nil
	}
	return map[string]any{"result": f.Result, "reasons": f.Reasons, "based_on_content_version": f.BasedOn}
}

// publish 发布 detection.signal.triaged.v1（只含改变的部分）并为处理状态的改变写审计。
func publish(ctx context.Context, tx pgx.Tx, o op, c *current, parts Parts, contentVersion, stateVersion int64, undoOf string, at time.Time) error {
	rc, _ := reqctx.From(ctx)
	ev := map[string]any{}
	if f := parts.Feedback; f != nil && f.Change == "changed" {
		part := map[string]any{"version": f.Version}
		if f.Before != nil {
			part["before"] = eventFeedback(f.Before)
		}
		if f.After != nil {
			part["after"] = eventFeedback(f.After)
		}
		ev["feedback"] = part
	}
	stateChanged := false
	if p := parts.Status; p != nil && p.Change == "changed" {
		ev["status"] = map[string]any{"before": p.Before, "after": p.After, "version": p.Version}
		stateChanged = true
	}
	if p := parts.Manual; p != nil && p.Change == "changed" {
		ev["manual_disposition"] = map[string]any{"before": p.Before, "after": p.After, "version": p.Version}
		stateChanged = true
	}
	action := o.req.Action
	if undoOf != "" {
		action = "undo"
	}
	channel := o.req.Channel
	if channel == "" {
		channel = "web"
	}
	data := map[string]any{"operation_id": o.id, "signal_id": c.id, "objective_id": c.objective, "actor": rc.Principal.ID, "action": action,
		"seen_content_version": o.req.SeenContent, "content_version": contentVersion, "disposition": c.disposition,
		"signal_created_at": c.createdAt.UTC().Format(time.RFC3339Nano), "parts": ev, "channel": channel, "occurred_at": at.Format(time.RFC3339Nano)}
	if o.batch != "" {
		data["batch_id"] = o.batch
	}
	if undoOf != "" {
		data["undo_of"] = undoOf
	}
	if action == "adjust" && o.req.Reason != "" {
		data["reason"] = o.req.Reason
	}
	if _, err := writer.Append(ctx, tx, outbox.Event{Type: SignalTriaged, Subject: c.id, PartitionKey: c.id, Data: data}); err != nil {
		return err
	}
	if !stateChanged {
		return nil
	}
	// 审计只含引用：操作标识指向处理记录（原因等内容在记录中），附内容版本与写入后的状态版本
	return recorder.Record(ctx, tx, audit.Entry{Action: "detection.signal." + action,
		Resource: fmt.Sprintf("%s/op:%s/content:%d/state:%d", c.id, o.id, contentVersion, stateVersion), Result: "succeeded", Basis: relFeedback})
}

// apply 写入计划中的变化。
func apply(ctx context.Context, tx pgx.Tx, rc reqctx.Context, c *current, p *plan, at time.Time) error {
	if p.stateChange {
		reason := c.manualReason
		var r *string
		if p.reason != nil {
			reason = *p.reason
		}
		if p.manual == "" {
			reason = ""
		}
		if reason != "" {
			r = &reason
		}
		var manual *string
		if p.manual != "" {
			manual = &p.manual
		}
		if _, err := tx.Exec(ctx, `UPDATE det_signals SET status = $2, manual_disposition = $3, manual_reason = $4, state_version = state_version + 1
 WHERE id = $1`, c.id, p.status, manual, r); err != nil {
			return err
		}
	}
	if p.fbChange {
		var result, note *string
		reasons, based := []string{}, int64(0)
		if p.fb != nil {
			result, reasons, based = &p.fb.Result, p.fb.Reasons, p.fb.BasedOn
			if p.fb.Note != "" {
				note = &p.fb.Note
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO det_feedback (tenant_id, signal_id, member, result, reasons, note, based_on_content_version, version, updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,1,$8) ON CONFLICT (signal_id, member) DO UPDATE SET result = EXCLUDED.result, reasons = EXCLUDED.reasons, note = EXCLUDED.note,
 based_on_content_version = EXCLUDED.based_on_content_version, version = det_feedback.version + 1, updated_at = EXCLUDED.updated_at`,
			rc.Tenant, c.id, rc.Principal.ID, result, reasons, note, based, at); err != nil {
			return err
		}
	}
	return nil
}

// run 执行一个操作：先按操作标识查找已固定的结果；读取信号并实时判断 can_feedback（判断失败为暂时失败，不固定）；然后在一个
// 事务中加锁、按部分判断并写入（确定失败与结果一起固定在操作标识上）。
func (s *Service) run(ctx context.Context, rc reqctx.Context, o op) (*Result, error) {
	var prev *stored
	var objective string
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if prev, err = readOp(ctx, tx, o.id); err != nil || prev != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT objective_id FROM det_signals WHERE id = $1`, o.signal).Scan(&objective)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if prev != nil {
		return replay[Result](prev, rc.Principal.ID, o.fingerprint)
	}
	if objective == "" || (o.objective != "" && objective != o.objective) {
		return fixFailure[Result](ctx, s, rc, o, definite(404, "signal_not_found"), nil)
	}
	o.objective = objective
	visible, granted, err := s.access(ctx, objective, relViewSignals, relFeedback)
	if err != nil {
		return nil, err
	}
	if !visible || !has(granted, relViewSignals) {
		return fixFailure[Result](ctx, s, rc, o, definite(404, "signal_not_found"), nil)
	}
	if !has(granted, relFeedback) {
		return fixFailure[Result](ctx, s, rc, o, definite(403, "forbidden"), nil)
	}
	var reasons *FeedbackReasons
	var out *Result
	var fixed *api.Error
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "det-op/"+rc.Tenant+"/"+o.id); err != nil {
			return err
		}
		prev, err := readOp(ctx, tx, o.id)
		if err != nil {
			return err
		}
		if prev != nil {
			out, err = replay[Result](prev, rc.Principal.ID, o.fingerprint)
			return err
		}
		c, err := lockSignal(ctx, tx, o.signal, rc.Principal.ID)
		if err != nil {
			return err
		}
		var code string
		if err := tx.QueryRow(ctx, `SELECT objective_code FROM det_signals WHERE id = $1`, o.signal).Scan(&code); err != nil {
			return err
		}
		if s.Packs != nil {
			reasons = s.Packs.Feedback[code]
		}
		at := s.now()
		p, e := decide(c, o.req, reasons)
		if e != nil {
			fixed = e
			return insertOp(ctx, tx, rc, o, "failed", e.Status, &e.Code, problemOf(e), nil, c.contentVersion, nil, at)
		}
		res := &Result{OperationID: o.id, SignalID: o.signal, Action: o.req.Action, Result: "no_change", Parts: p.parts}
		if p.parts.changed() {
			res.Result = "succeeded"
		}
		if err := apply(ctx, tx, rc, c, p, at); err != nil {
			return err
		}
		after, err := lockSignal(ctx, tx, o.signal, rc.Principal.ID)
		if err != nil {
			return err
		}
		res.State = after.state()
		response, _ := json.Marshal(res)
		parts, _ := json.Marshal(p.parts)
		if err := insertOp(ctx, tx, rc, o, res.Result, 200, nil, response, parts, c.contentVersion, nil, at); err != nil {
			return err
		}
		if res.Result == "succeeded" {
			if err := publish(ctx, tx, o, c, p.parts, c.contentVersion, after.stateVersion, "", at); err != nil {
				return err
			}
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	if fixed != nil {
		return nil, fixed
	}
	return out, nil
}

// attempt 以（调用者、接口、幂等键）记录一次尝试：同键同请求返回该尝试的结果（含失败），同键不同请求 409 idempotency_conflict。
// 成功与失败都记录；本库不可用时无法记录失败的尝试（同键重发会再次执行，已提交的操作仍按操作标识返回已固定的结果）。
func (s *Service) attempt(ctx context.Context, rc reqctx.Context, scope, key, fp string, status int, fn func() (any, error)) ([]byte, error) {
	var prev []byte
	var prevStatus int
	var prevFP string
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT fingerprint, status, response FROM det_triage_attempts WHERE actor = $1 AND scope = $2 AND idem_key = $3`,
			rc.Principal.ID, scope, key).Scan(&prevFP, &prevStatus, &prev)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if prev != nil {
		return replayAttempt(prevFP, fp, prevStatus, prev)
	}
	out, runErr := fn()
	var body []byte
	code := status
	var safe *api.Error
	switch {
	case runErr == nil:
		body, _ = json.Marshal(out)
	case errors.As(runErr, &safe):
		code, body = safe.Status, problemOf(safe)
	case errors.Is(runErr, reqctx.ErrMissing):
		return nil, runErr
	default:
		s.logger().WarnContext(ctx, "处理操作失败", "scope", scope, "error", runErr)
		safe = &api.Error{Status: 503, Code: "detection_unavailable"}
		code, body, runErr = 503, problemOf(safe), safe
	}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO det_triage_attempts (tenant_id, actor, scope, idem_key, fingerprint, status, response) VALUES ($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT DO NOTHING`, rc.Tenant, rc.Principal.ID, scope, key, fp, code, body)
		if err != nil || tag.RowsAffected() == 1 {
			return err
		}
		// 并发的同键尝试已先记录：返回先记录的结果
		return tx.QueryRow(ctx, `SELECT fingerprint, status, response FROM det_triage_attempts WHERE actor = $1 AND scope = $2 AND idem_key = $3`,
			rc.Principal.ID, scope, key).Scan(&prevFP, &prevStatus, &prev)
	})
	if err != nil {
		s.logger().WarnContext(ctx, "尝试结果未能记录", "scope", scope)
	}
	if prev != nil {
		return replayAttempt(prevFP, fp, prevStatus, prev)
	}
	if runErr != nil {
		return nil, runErr
	}
	return body, nil
}

func replayAttempt(prevFP, fp string, status int, body []byte) ([]byte, error) {
	if prevFP != fp {
		return nil, definite(409, "idempotency_conflict")
	}
	if status >= 400 {
		return nil, problemError(status, body)
	}
	return body, nil
}

// TriageSignal 执行单条反馈、忽略、恢复或调整处置（契约 triageSignal）。
func (s *Service) TriageSignal(ctx context.Context, signal, key string, req Request) (*Result, error) {
	rc, err := userContext(ctx)
	if err != nil {
		return nil, err
	}
	fp := fingerprint("triage", signal, req)
	body, err := s.attempt(ctx, rc, "triage", key, fp, 200, func() (any, error) {
		return s.run(ctx, rc, op{id: req.OperationID, fingerprint: fp, signal: signal, req: req})
	})
	if err != nil {
		return nil, err
	}
	var out Result
	return &out, json.Unmarshal(body, &out)
}
