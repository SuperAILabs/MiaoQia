package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 分派规则（customer-context.md 第11节，PRD 第6段 F1 第2条，C-14）。
//
//   - 存储：ctx_assignment_rules 每个目标一行，整份替换；分派配置版本独立于目标版本（从未配置为 0），不改变租户上下文版本——
//     规则不是策略求值的输入，不影响处置；detection 在信号上记录生成时所用的分派配置版本；
//   - 写入：目标单元的决策者（can_manage_participants 与 can_edit，与调整负责人和参与人相同）。每个被分派人先以其本人判断仍是
//     本租户有效成员且能查看该目标的信号（identity 会话核验 + 本子系统的 can_view_signals 口径），再在目标行锁（与 ConfigureObjective
//     同一把锁）下核对仍是目标当前的负责人或参与人；不满足为 422 assignee_no_access，不写入、不授予权限；
//   - 幂等：同一键重放返回首次结果，包括确定失败（4xx，见 commandOutcome）；
//   - 读取：字段按调用者级别的白名单（server.go 的 assignmentFields）；生效策略读取（服务主体）带全文，供 detection 生成信号时计算候选。
//
// 被分派人之后被移出目标时，ConfigureObjective 在同一目标行锁下移除指向他的规则（pruneAssignment，分派配置版本加一）；detection 计算时
// 另外只从同一次读取中的负责人与参与人中选（纵深防御）。候选在使用时（pursuit 的创建面板）仍以候选本人再检查，存储的候选只是提示，
// 不固定权限。

// Assignment 是目标的分派配置（契约 AssignmentRules 的全文）。
type Assignment struct {
	ObjectiveID string                  `json:"objective_id"`
	Version     int64                   `json:"version"`
	Rules       []domain.AssignmentRule `json:"rules"`
	UpdatedAt   *time.Time              `json:"updated_at,omitempty"`
}

// 读取级别：full 带规则全文与更新时间；summary 只带版本与规则条数。
const (
	AssignmentFull    = "full"
	AssignmentSummary = "summary"
)

// readAssignment 读取目标的分派配置；从未配置时为版本 0、没有规则。
func readAssignment(ctx context.Context, tx pgx.Tx, objective string) (*Assignment, error) {
	out := &Assignment{ObjectiveID: objective, Rules: []domain.AssignmentRule{}}
	var raw []byte
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT version, rules, updated_at FROM ctx_assignment_rules WHERE objective_id = $1`, objective).Scan(&out.Version, &raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out.Rules); err != nil {
		return nil, err
	}
	at = at.UTC()
	out.UpdatedAt = &at
	return out, nil
}

// GetAssignment 读取目标的分派规则（can_view；不可见与不存在同为 404）。返回调用者的读取级别：能管理参与人或能分派跟进的
// 成员为 full，其余为 summary。
func (s *Service) GetAssignment(ctx context.Context, id string) (*Assignment, string, error) {
	rc, o, err := s.requireObjective(ctx, id, "can_view")
	if err != nil {
		return nil, "", err
	}
	level := AssignmentSummary
	for _, relation := range []string{"can_manage_participants", "can_assign"} {
		ok, err := s.allowedObjective(ctx, rc, o, relation)
		if err != nil {
			return nil, "", err
		}
		if ok {
			level = AssignmentFull
			break
		}
	}
	var out *Assignment
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = readAssignment(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return out, level, nil
}

// assigneeAccess 以被分派人本人判断其此刻是否仍是本租户有效成员（identity 会话核验：退出、停用或他租户的用户为 403）且能查看
// 目标的信号（本子系统的 can_view_signals 口径：单元关系，或目标当前版本的负责人/参与人且 identity 判断通过）。明确否定返回 false；
// 判断失败为 503（失败关闭，不当作无权限也不当作有权限）。
func (s *Service) assigneeAccess(ctx context.Context, rc reqctx.Context, o *Objective, subject string) (bool, error) {
	if s.Sessions == nil {
		return false, failure(503, "identity_unavailable")
	}
	mrc := reqctx.Context{Tenant: rc.Tenant, Cell: rc.Cell, Principal: reqctx.Principal{ID: subject, Kind: "user"}, Purpose: rc.Purpose}
	mctx := reqctx.With(ctx, mrc)
	if _, err := s.Sessions.Resolve(mctx); err != nil {
		var safe *identity.Error
		if errors.As(err, &safe) && safe.Status == 403 {
			return false, nil
		}
		return false, failure(503, "identity_unavailable")
	}
	return s.allowedObjective(mctx, mrc, o, "can_view_signals")
}

// UpdateAssignment 整份替换目标的分派规则（契约 updateAssignmentRules）。
func (s *Service) UpdateAssignment(ctx context.Context, id string, expected int64, rules []domain.AssignmentRule, key string) (*Assignment, error) {
	rc, o, err := s.requireObjective(ctx, id, "can_manage_participants", "can_edit")
	if err != nil {
		return nil, err
	}
	if rules == nil {
		rules = []domain.AssignmentRule{}
	}
	for _, r := range rules {
		if !subjectPattern.MatchString(r.Assignee) {
			return nil, failure(400, "invalid_request")
		}
	}
	if expected < 0 {
		return nil, failure(400, "invalid_request")
	}
	request := struct {
		ID       string                  `json:"id"`
		Expected int64                   `json:"expected_version"`
		Rules    []domain.AssignmentRule `json:"rules"`
	}{id, expected, rules}
	out := &Assignment{}
	pre := func() error {
		if err := domain.ValidateAssignment(s.Catalog, rules); err != nil {
			var bad *domain.Invalid
			if errors.As(err, &bad) {
				return failureDetail(422, bad.Code, bad.Detail)
			}
			return err
		}
		for _, a := range domain.Assignees(rules) {
			if !o.involved(a) {
				return failureDetail(422, "assignee_no_access", a)
			}
			ok, err := s.assigneeAccess(ctx, rc, o, a)
			if err != nil {
				return err
			}
			if !ok {
				return failureDetail(422, "assignee_no_access", a)
			}
		}
		return nil
	}
	err = s.commandOutcome(ctx, key, "objective.assignment", request, out, pre, func(ctx context.Context, tx pgx.Tx) error {
		// 与 ConfigureObjective 同一把锁（目标行）：锁下再核对被分派人仍是目标当前的负责人或参与人，参与人的移除与规则的写入串行
		locked, err := scanObjective(tx.QueryRow(ctx, `SELECT `+objectiveColumns+` FROM ctx_objectives WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "objective_not_found")
		}
		if err != nil {
			return err
		}
		for _, a := range domain.Assignees(rules) {
			if !locked.involved(a) {
				return failureDetail(422, "assignee_no_access", a)
			}
		}
		current, err := readAssignment(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.Version != expected {
			return failureDetail(409, "version_conflict", fmt.Sprintf("当前分派配置版本 %d", current.Version))
		}
		if sameRules(current.Rules, rules) && current.Version > 0 {
			*out = *current
			return nil
		}
		raw, err := json.Marshal(rules)
		if err != nil {
			return err
		}
		next := current.Version + 1
		var at time.Time
		if err := tx.QueryRow(ctx, `INSERT INTO ctx_assignment_rules (objective_id, tenant_id, version, rules, updated_by) VALUES ($1, $2, $3, $4, $5)
 ON CONFLICT (objective_id) DO UPDATE SET version = EXCLUDED.version, rules = EXCLUDED.rules, updated_by = EXCLUDED.updated_by, updated_at = now()
 RETURNING updated_at`, id, rc.Tenant, next, raw, rc.Principal.ID).Scan(&at); err != nil {
			return err
		}
		at = at.UTC()
		*out = Assignment{ObjectiveID: id, Version: next, Rules: slices.Clone(rules), UpdatedAt: &at}
		// 审计只记引用：规则全文是该分派配置版本
		return recorder.Record(ctx, tx, audit.Entry{Action: "customer_context.objective.assignment_configured",
			Resource: fmt.Sprintf("%s/assignment/v%d/rules:%d", id, next, len(rules)), Result: "succeeded", Basis: "can_edit"})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pruneAssignment 在调整负责人与参与人的同一事务、同一目标行锁下移除被分派人已不是负责人或参与人的规则；有移除时分派配置版本加一并审计
// （之后以旧版本写入的请求 409 version_conflict）。
func pruneAssignment(ctx context.Context, tx pgx.Tx, rc reqctx.Context, o *Objective) error {
	current, err := readAssignment(ctx, tx, o.ID)
	if err != nil || current.Version == 0 {
		return err
	}
	kept := slices.DeleteFunc(slices.Clone(current.Rules), func(r domain.AssignmentRule) bool { return !o.involved(r.Assignee) })
	if len(kept) == len(current.Rules) {
		return nil
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	next := current.Version + 1
	if _, err := tx.Exec(ctx, `UPDATE ctx_assignment_rules SET version = $2, rules = $3, updated_by = $4, updated_at = now() WHERE objective_id = $1`,
		o.ID, next, raw, rc.Principal.ID); err != nil {
		return err
	}
	return recorder.Record(ctx, tx, audit.Entry{Action: "customer_context.objective.assignment_pruned",
		Resource: fmt.Sprintf("%s/assignment/v%d/rules:%d", o.ID, next, len(kept)), Result: "succeeded", Basis: "can_manage_participants"})
}

func sameRules(a, b []domain.AssignmentRule) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// commandOutcome 与 command 相同（同一主体、租户与键重放返回首次结果；键相同而操作或请求不同 409 idempotency_conflict），另把
// 确定失败（4xx）也固定在键上：重放返回首次的失败，不再执行。暂时失败（503 等）不登记，同一键可重试。
//
// pre 在事务外执行（例如经 identity 以被分派人本人判断访问）；apply 在登记结果的同一事务中执行。失败结果在单独的事务中登记，
// 登记前再查一次（并发的同键请求已登记时返回已登记的结果）。
func (s *Service) commandOutcome(ctx context.Context, key, operation string, request, response any, pre func() error,
	apply func(context.Context, pgx.Tx) error) error {
	if !commandKey.MatchString(key) {
		return failure(400, "invalid_idempotency_key")
	}
	var stored error
	found := false
	if err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, found, stored, err = replayOutcome(ctx, tx, key, operation, request, response)
		return err
	}); err != nil {
		return err
	}
	if found {
		return stored
	}
	replayed := false
	err := pre()
	if err == nil {
		err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			fingerprint, found, stored, err := replayOutcome(ctx, tx, key, operation, request, response)
			if err != nil {
				return err
			}
			if found {
				replayed = true
				return stored
			}
			if err := apply(ctx, tx); err != nil {
				return err
			}
			return recordOutcome(ctx, tx, key, operation, fingerprint, response, nil)
		})
	}
	var safe *api.Error
	if replayed || !errors.As(err, &safe) || safe.Status < 400 || safe.Status >= 500 || safe.Code == "idempotency_conflict" {
		return err
	}
	result := err
	if rerr := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		fingerprint, found, stored, err := replayOutcome(ctx, tx, key, operation, request, response)
		if err != nil {
			return err
		}
		if found {
			result = stored
			return nil
		}
		return recordOutcome(ctx, tx, key, operation, fingerprint, nil, safe)
	}); rerr != nil {
		s.logger().WarnContext(ctx, "确定失败未能登记到幂等键", "subsystem", api.Name, "operation", operation, "error", rerr.Error())
	}
	return result
}

// outcome 是 commandOutcome 登记的结果：成功的响应或确定失败。
type outcome struct {
	OK      json.RawMessage `json:"ok,omitempty"`
	Failure *storedFailure  `json:"failure,omitempty"`
}

type storedFailure struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// replayOutcome 与 replay 相同（同一把事务级咨询锁），读取 commandOutcome 登记的结果：成功时填入 response，失败时返回该失败。
func replayOutcome(ctx context.Context, tx pgx.Tx, key, operation string, request, response any) (string, bool, error, error) {
	var raw json.RawMessage
	fingerprint, found, err := replay(ctx, tx, key, operation, request, &raw)
	if err != nil || !found {
		return fingerprint, false, nil, err
	}
	var o outcome
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", false, nil, err
	}
	if o.Failure != nil {
		return fingerprint, true, failureDetail(o.Failure.Status, o.Failure.Code, o.Failure.Detail), nil
	}
	return fingerprint, true, nil, json.Unmarshal(o.OK, response)
}

func recordOutcome(ctx context.Context, tx pgx.Tx, key, operation, fingerprint string, response any, fail *api.Error) error {
	rc, _ := reqctx.From(ctx)
	var o outcome
	if fail != nil {
		o.Failure = &storedFailure{Status: fail.Status, Code: fail.Code, Detail: fail.Detail}
	} else {
		raw, err := json.Marshal(response)
		if err != nil {
			return err
		}
		o.OK = raw
	}
	encoded, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ctx_commands (tenant_id, actor, request_key, operation, fingerprint, response) VALUES ($1, $2, $3, $4, $5, $6)`,
		rc.Tenant, rc.Principal.ID, key, operation, fingerprint, encoded)
	return err
}
