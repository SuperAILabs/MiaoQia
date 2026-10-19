package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 激活与回滚申请（PRD 第7段 F3–F5；customer-context.md 第6节“策略激活与回滚”）。审批本身在 identity（四眼、强认证、7天有效期、
// 申请人退出或失去申请关系时失效）；本子系统负责：
//
//	登记：申请前核对草案、仿真与基准；经 identity.RequestApproval 创建审批后，在策略状态行锁下重新核对绑定并登记本地申请
//	      （激活申请同时锁定草案）。核对不成立时经 CloseApproval 使刚创建的审批失效（object_changed / baseline_changed）。
//	      identity 已创建而本地登记未完成（进程退出、数据库暂不可用）时，由 identity.approval.requested 的消费处理补登或使其失效。
//	执行：消费 identity.approval.decided。批准只针对绑定的草案修订（或目标版本）、仿真运行与申请时的生效版本和策略代次；
//	      执行前在同一事务中重新核验，不成立时不执行，申请记为 failed 并发布 policy.execution_failed。
//	关闭：消费 identity.approval.closed，申请记为 withdrawn / expired / invalidated 并解锁草案。
//
// 事件按资源分区（同一目标的审批事件有序），处理按申请状态幂等。
const (
	kindActivation = "activation"
	kindRollback   = "rollback"
	// defaultBackfillDays 是首次激活未给出回填天数时的缺省值（PRD 第7段第13节第4项）。
	defaultBackfillDays = 30
)

// approvalRef 是审批请求在本子系统用到的部分（来自 identity 的返回或审批事件）。
type approvalRef struct {
	ID, Kind, Objective, Applicant, State string
	ObjectVersion                         int64
	Evidence                              string
	BaselineVersion, BaselineGeneration   int64
	Backfill                              *int
}

func kindOf(approvalType string) (string, bool) {
	switch approvalType {
	case string(identityapi.ApprovalTypePolicyActivation):
		return kindActivation, true
	case string(identityapi.ApprovalTypePolicyRollback):
		return kindRollback, true
	}
	return "", false
}

func objectiveOf(resource string) (string, bool) {
	id, ok := strings.CutPrefix(resource, "objective:")
	return id, ok && strings.HasPrefix(id, "obj_")
}

func refFromApproval(a *identity.ApprovalRequest) (approvalRef, error) {
	kind, ok := kindOf(string(a.Type))
	objective, rok := objectiveOf(a.Resource)
	if !ok || !rok {
		return approvalRef{}, fmt.Errorf("customer-context：审批请求 %s 不是策略申请", a.ID)
	}
	ref := approvalRef{ID: string(a.ID), Kind: kind, Objective: objective, Applicant: a.Applicant, State: string(a.State),
		ObjectVersion: a.Binding.ObjectVersion, Evidence: a.Binding.EvidenceRef, BaselineVersion: a.Binding.BaselineVersion,
		BaselineGeneration: a.Binding.BaselineGeneration}
	if d, ok := a.Binding.BackfillDays.Get(); ok {
		ref.Backfill = &d
	}
	return ref, nil
}

// approvalEvent 是 identity.approval.*.v1 的载荷中本子系统用到的字段。
type approvalEvent struct {
	TenantID   string `json:"tenant_id"`
	ApprovalID string `json:"approval_id"`
	Type       string `json:"type"`
	Resource   string `json:"resource"`
	Applicant  string `json:"applicant"`
	Binding    struct {
		ObjectVersion      int64  `json:"object_version"`
		EvidenceRef        string `json:"evidence_ref"`
		BaselineVersion    int64  `json:"baseline_version"`
		BaselineGeneration int64  `json:"baseline_generation"`
		BackfillDays       *int   `json:"backfill_days"`
	} `json:"binding"`
	EffectiveAt time.Time `json:"effective_at"`
	Decision    string    `json:"decision"`
	DecidedBy   string    `json:"decided_by"`
	Reason      string    `json:"reason"`
	State       string    `json:"state"`
}

// parseApprovalEvent 解析策略审批事件；不是策略审批（其他类型）时返回 nil。事件须带与载荷一致的租户上下文。
func parseApprovalEvent(env *outbox.Envelope) (*approvalEvent, *approvalRef, error) {
	var e approvalEvent
	if err := json.Unmarshal(env.Data, &e); err != nil {
		return nil, nil, err
	}
	kind, ok := kindOf(e.Type)
	if !ok {
		return nil, nil, nil
	}
	objective, ok := objectiveOf(e.Resource)
	if !ok {
		return nil, nil, fmt.Errorf("customer-context：审批事件 %s 的资源 %s 不是目标", env.ID, e.Resource)
	}
	if env.Tenant == "" || env.Cell == "" || env.Tenant != e.TenantID {
		// 不带租户上下文无法按行级安全定位策略；不静默跳过
		return nil, nil, fmt.Errorf("customer-context：审批事件 %s 的租户上下文缺失或不一致", env.ID)
	}
	return &e, &approvalRef{ID: e.ApprovalID, Kind: kind, Objective: objective, Applicant: e.Applicant, State: "pending",
		ObjectVersion: e.Binding.ObjectVersion, Evidence: e.Binding.EvidenceRef, BaselineVersion: e.Binding.BaselineVersion,
		BaselineGeneration: e.Binding.BaselineGeneration, Backfill: e.Binding.BackfillDays}, nil
}

// readRequest 读取本地申请记录（可加排他锁）；不存在返回 nil。
func readRequest(ctx context.Context, tx pgx.Tx, approvalID string, lock bool) (*RequestRec, error) {
	q := `SELECT ` + requestColumns + ` FROM ctx_policy_requests WHERE approval_id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	r, err := scanRequest(tx.QueryRow(ctx, q, approvalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func pendingRequest(ctx context.Context, tx pgx.Tx, objective string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT approval_id FROM ctx_policy_requests WHERE objective_id = $1 AND state = 'pending'`, objective).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// checkBinding 核对审批绑定在当前状态下是否仍成立（登记与执行共用；调用方持有目标行与策略状态行的锁）。
// 不成立时返回原因：objective_disabled、baseline_changed、object_changed、version_missing（与 policy.execution_failed 一致）。
func checkBinding(ctx context.Context, tx pgx.Tx, o *Objective, p *policyRow, ref approvalRef) (string, error) {
	if o.State == domain.StateDisabled {
		return "objective_disabled", nil
	}
	if p.active != ref.BaselineVersion || p.generation != ref.BaselineGeneration {
		return "baseline_changed", nil
	}
	if ref.Kind == kindRollback {
		var simulation, state string
		err := tx.QueryRow(ctx, `SELECT simulation_id, state FROM ctx_policy_versions WHERE objective_id = $1 AND version = $2`, o.ID, ref.ObjectVersion).Scan(&simulation, &state)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (state == "active" || ref.ObjectVersion == p.active)) {
			return "version_missing", nil
		}
		if err != nil {
			return "", err
		}
		if simulation != ref.Evidence || ref.Backfill != nil {
			return "object_changed", nil
		}
		return "", nil
	}
	// 激活：绑定的修订仍是当前草案，草案未被其他申请锁定，仿真运行是该修订已完成的仿真且对比一侧等于基准
	if p.draftRevision == nil || *p.draftRevision != ref.ObjectVersion || (p.locked() != "" && p.locked() != ref.ID) {
		return "object_changed", nil
	}
	if ref.Backfill != nil && ref.BaselineVersion != 0 {
		return "object_changed", nil
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT valid FROM ctx_policy_revisions WHERE objective_id = $1 AND revision = $2`, o.ID, ref.ObjectVersion).Scan(&valid); err != nil {
		return "", err
	}
	var status string
	var compareVersion, compareGeneration int64
	err := tx.QueryRow(ctx, `SELECT status, compare_version, compare_generation FROM ctx_policy_simulations WHERE simulation_id = $1 AND objective_id = $2 AND revision = $3`,
		ref.Evidence, o.ID, ref.ObjectVersion).Scan(&status, &compareVersion, &compareGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return "object_changed", nil
	}
	if err != nil {
		return "", err
	}
	if !valid || status != "completed" || compareVersion != ref.BaselineVersion || compareGeneration != ref.BaselineGeneration {
		return "object_changed", nil
	}
	return "", nil
}

// closeReason 把核对原因映射为 identity CloseApproval 的原因。
func closeReason(mismatch string) identityapi.CloseApprovalRequestReason {
	if mismatch == "baseline_changed" {
		return identityapi.CloseApprovalRequestReasonBaselineChanged
	}
	return identityapi.CloseApprovalRequestReasonObjectChanged
}

// adoptLocked 登记待审批的申请（调用方持有目标行共享锁与策略状态行锁）：已登记时返回已有记录；绑定不成立、请求已不在待审批或
// 目标已有其他待审批申请时返回不成立的原因，不登记。
func adoptLocked(ctx context.Context, tx pgx.Tx, tenant string, o *Objective, p *policyRow, ref approvalRef) (*RequestRec, string, error) {
	existing, err := readRequest(ctx, tx, ref.ID, false)
	if err != nil || existing != nil {
		return existing, "", err
	}
	if ref.State != string(identityapi.ApprovalStatePending) {
		return nil, "object_changed", nil
	}
	mismatch, err := checkBinding(ctx, tx, o, p, ref)
	if err != nil || mismatch != "" {
		return nil, mismatch, err
	}
	other, err := pendingRequest(ctx, tx, o.ID)
	if err != nil {
		return nil, "", err
	}
	if other != "" {
		return nil, "object_changed", nil
	}
	rec := &RequestRec{ApprovalID: ref.ID, Kind: ref.Kind, State: "pending", ObjectVersion: ref.ObjectVersion, SimulationID: ref.Evidence,
		BaselineVersion: ref.BaselineVersion, BaselineGeneration: ref.BaselineGeneration, BackfillDays: ref.Backfill, RequestedBy: ref.Applicant}
	if _, err := tx.Exec(ctx, `INSERT INTO ctx_policy_requests (approval_id, tenant_id, objective_id, kind, state, object_version, simulation_id, baseline_version, baseline_generation, backfill_days, requested_by)
 VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7, $8, $9, $10)`, rec.ApprovalID, tenant, o.ID, rec.Kind, rec.ObjectVersion, rec.SimulationID, rec.BaselineVersion,
		rec.BaselineGeneration, rec.BackfillDays, rec.RequestedBy); err != nil {
		return nil, "", err
	}
	if ref.Kind == kindActivation {
		if _, err := tx.Exec(ctx, `UPDATE ctx_policies SET draft_locked_by = $2, updated_at = now() WHERE objective_id = $1`, o.ID, ref.ID); err != nil {
			return nil, "", err
		}
		p.lockedBy = &ref.ID
	}
	return rec, "", recorder.Record(ctx, tx, audit.Entry{Action: "customer_context.policy." + ref.Kind + "_requested",
		Resource: fmt.Sprintf("%s/%s", o.ID, ref.ID), Result: "succeeded", Basis: "can_request_activation"})
}

// identityFailure 把 identity 的错误转换为本子系统的错误：业务错误（强认证、无权限、进行中的审批等）原样传递，其余为 503。
func identityFailure(err error) error {
	var safe *identity.Error
	if errors.As(err, &safe) && safe.Status < 500 {
		return failure(safe.Status, safe.Code)
	}
	return failure(503, "identity_unavailable")
}

// closeApproval 以本子系统的服务主体使审批请求失效（幂等）。失败只记录：待审批的请求仍由执行前核验与
// identity.approval.requested 的对账兜底，不会被错误执行。
func (s *Service) closeApproval(ctx context.Context, approvalID, mismatch string) {
	if s.Approvals == nil {
		return
	}
	sctx := reqctx.Service(ctx, api.Name)
	if _, err := s.Approvals.CloseApproval(sctx, approvalID, &identity.CloseApprovalRequest{Reason: closeReason(mismatch)}); err != nil {
		s.logger().Warn("审批请求失效未完成，由对账重试", "approval", approvalID, "reason", mismatch, "error", err)
	}
}

// ActivationInput 是提交激活申请的输入。
type ActivationInput struct {
	Expected     int64  `json:"expected_version"`
	Revision     int64  `json:"revision"`
	SimulationID string `json:"simulation_id"`
	Note         string `json:"note"`
	BackfillDays *int   `json:"backfill_days,omitempty"`
}

// RequestPolicyActivation 提交激活申请（can_request_activation；契约 requestPolicyActivation）。
func (s *Service) RequestPolicyActivation(ctx context.Context, id string, in ActivationInput, key string) (*RequestRec, error) {
	if in.Expected < 1 || in.Revision < 1 || strings.TrimSpace(in.Note) == "" || (in.BackfillDays != nil && (*in.BackfillDays < 0 || *in.BackfillDays > 30)) {
		return nil, failure(400, "invalid_request")
	}
	rc, _, err := s.requireObjective(ctx, id, "can_request_activation")
	if err != nil {
		return nil, err
	}
	request := struct {
		ID    string          `json:"id"`
		Input ActivationInput `json:"input"`
	}{id, in}
	var binding identity.ApprovalBinding
	precheck := func(ctx context.Context, tx pgx.Tx, o *Objective, p *policyRow) error {
		if in.BackfillDays != nil && p.active > 0 {
			return failure(422, "backfill_not_allowed")
		}
		if p.draftRevision == nil || p.draftVersion != in.Expected || *p.draftRevision != in.Revision {
			return failureDetail(409, "version_conflict", fmt.Sprintf("当前草案版本 %d", p.draftVersion))
		}
		if p.locked() != "" {
			return failure(409, "draft_locked")
		}
		r, err := readRevision(ctx, tx, id, in.Revision)
		if err != nil {
			return err
		}
		if !r.valid {
			return failure(409, "draft_invalid")
		}
		var status string
		var compareVersion, compareGeneration int64
		err = tx.QueryRow(ctx, `SELECT status, compare_version, compare_generation FROM ctx_policy_simulations WHERE simulation_id = $1 AND objective_id = $2 AND revision = $3`,
			in.SimulationID, id, in.Revision).Scan(&status, &compareVersion, &compareGeneration)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "completed") {
			return failure(409, "simulation_not_ready")
		}
		if err != nil {
			return err
		}
		if compareVersion != p.active || compareGeneration != p.generation {
			return failure(409, "simulation_stale")
		}
		binding = identity.ApprovalBinding{ObjectVersion: in.Revision, EvidenceRef: in.SimulationID, BaselineVersion: p.active, BaselineGeneration: p.generation}
		if p.active == 0 {
			days := defaultBackfillDays
			if in.BackfillDays != nil {
				days = *in.BackfillDays
			}
			binding.BackfillDays = identityapi.NewOptInt(days)
		}
		return nil
	}
	return s.submitRequest(ctx, rc, id, "policy.activation", request, identityapi.ApprovalTypePolicyActivation, in.Note, &binding, precheck, key)
}

// RequestPolicyRollback 提交回滚申请（can_request_activation；契约 requestPolicyRollback）：回到曾经生效、且不是当前生效的版本。
func (s *Service) RequestPolicyRollback(ctx context.Context, id string, target int64, note, key string) (*RequestRec, error) {
	if target < 1 || strings.TrimSpace(note) == "" {
		return nil, failure(400, "invalid_request")
	}
	rc, _, err := s.requireObjective(ctx, id, "can_request_activation")
	if err != nil {
		return nil, err
	}
	request := struct {
		ID     string `json:"id"`
		Target int64  `json:"target_version"`
		Note   string `json:"note"`
	}{id, target, note}
	var binding identity.ApprovalBinding
	precheck := func(ctx context.Context, tx pgx.Tx, o *Objective, p *policyRow) error {
		v, err := readVersion(ctx, tx, id, target)
		if err != nil {
			return err
		}
		if v.State == "active" || target == p.active {
			return failure(409, "already_active")
		}
		binding = identity.ApprovalBinding{ObjectVersion: target, EvidenceRef: v.SimulationID, BaselineVersion: p.active, BaselineGeneration: p.generation}
		return nil
	}
	return s.submitRequest(ctx, rc, id, "policy.rollback", request, identityapi.ApprovalTypePolicyRollback, note, &binding, precheck, key)
}

// submitRequest 是激活与回滚申请的共同流程：重放检查与申请前核对 → identity.RequestApproval（同一幂等键）→ 在策略状态行锁下
// 登记；登记时绑定已不成立则使刚创建的审批失效并返回 409 version_conflict。
func (s *Service) submitRequest(ctx context.Context, rc reqctx.Context, id, operation string, request any, approvalType identityapi.ApprovalType, note string,
	binding *identity.ApprovalBinding, precheck func(context.Context, pgx.Tx, *Objective, *policyRow) error, key string) (*RequestRec, error) {
	if s.Approvals == nil {
		return nil, failure(503, "identity_unavailable")
	}
	out := &RequestRec{}
	var replayed bool
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, found, err := replay(ctx, tx, key, operation, request, out)
		if err != nil || found {
			replayed = found
			return err
		}
		o, err := readObjectiveShared(ctx, tx, id)
		if err != nil {
			return err
		}
		if o.State == domain.StateDisabled {
			return failure(409, "objective_disabled")
		}
		p, err := readPolicyRow(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := precheck(ctx, tx, o, p); err != nil {
			return err
		}
		pending, err := pendingRequest(ctx, tx, id)
		if err != nil {
			return err
		}
		if pending != "" {
			return failureDetail(409, "approval_in_progress", pending)
		}
		return nil
	})
	if err != nil || replayed {
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	a, err := s.Approvals.RequestApproval(ctx, &identity.RequestApprovalRequest{Type: approvalType, Resource: "objective:" + id, Note: note, Binding: *binding}, key)
	if err != nil {
		return nil, identityFailure(err)
	}
	ref, err := refFromApproval(a)
	if err != nil || ref.Objective != id {
		return nil, failure(503, "identity_unavailable")
	}
	var mismatch string
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, operation, request, out, func() error {
			o, err := readObjectiveShared(ctx, tx, id)
			if err != nil {
				return err
			}
			p, err := lockPolicy(ctx, tx, rc.Tenant, id)
			if err != nil {
				return err
			}
			rec, m, err := adoptLocked(ctx, tx, rc.Tenant, o, p, ref)
			if err != nil {
				return err
			}
			if m != "" {
				mismatch = m
				if m == "objective_disabled" {
					return failure(409, "objective_disabled")
				}
				return failureDetail(409, "version_conflict", "提交期间草案、生效版本或目标状态已变化，审批请求已失效")
			}
			*out = *rec
			return nil
		})
	})
	if mismatch != "" {
		s.closeApproval(ctx, ref.ID, mismatch)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ApprovalRequested 是 identity.approval.requested.v1 的消费处理（对账）：本地尚未登记的策略申请按当前状态补登，绑定已不成立时
// 使其失效。已登记（申请命令已完成）时不做任何事。
func (s *Service) ApprovalRequested(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	_, ref, err := parseApprovalEvent(env)
	if err != nil || ref == nil {
		return err
	}
	o, err := readObjectiveShared(ctx, tx, ref.Objective)
	var missing *api.Error
	if errors.As(err, &missing) && missing.Status == 404 {
		return nil // 目标不存在（不应发生）：确认事件
	}
	if err != nil {
		return err
	}
	p, err := lockPolicy(ctx, tx, env.Tenant, ref.Objective)
	if err != nil {
		return err
	}
	existing, err := readRequest(ctx, tx, ref.ID, false)
	if err != nil || existing != nil {
		return err
	}
	_, mismatch, err := adoptLocked(ctx, tx, env.Tenant, o, p, *ref)
	if err != nil || mismatch == "" {
		return err
	}
	// 绑定不成立：使审批失效（幂等；请求已决定或已关闭时 identity 原样返回，随后的 decided / closed 事件照常处理）
	if s.Approvals == nil {
		return failure(503, "identity_unavailable")
	}
	if _, err := s.Approvals.CloseApproval(reqctx.Service(ctx, api.Name), ref.ID, &identity.CloseApprovalRequest{Reason: closeReason(mismatch)}); err != nil {
		var safe *identity.Error
		if errors.As(err, &safe) && safe.Status == 404 {
			return nil
		}
		return err
	}
	return nil
}

// ApprovalDecided 是 identity.approval.decided.v1 的消费处理：驳回时关闭申请并解锁草案；批准时执行前核验绑定，成立则激活或回滚，
// 不成立则记为 failed 并发布 policy.execution_failed。本地尚未登记的申请（登记前即被决定）按事件中的绑定补记后同样处理。
func (s *Service) ApprovalDecided(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	e, ref, err := parseApprovalEvent(env)
	if err != nil || ref == nil {
		return err
	}
	o, err := scanObjective(tx.QueryRow(ctx, `SELECT `+objectiveColumns+` FROM ctx_objectives WHERE id = $1 FOR UPDATE`, ref.Objective))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p, err := lockPolicy(ctx, tx, env.Tenant, ref.Objective)
	if err != nil {
		return err
	}
	rec, err := readRequest(ctx, tx, ref.ID, true)
	if err != nil {
		return err
	}
	fresh := rec == nil
	if fresh {
		rec = &RequestRec{ApprovalID: ref.ID, Kind: ref.Kind, State: "pending", ObjectVersion: ref.ObjectVersion, SimulationID: ref.Evidence,
			BaselineVersion: ref.BaselineVersion, BaselineGeneration: ref.BaselineGeneration, BackfillDays: ref.Backfill, RequestedBy: ref.Applicant}
	}
	if rec.State != "pending" {
		return nil // 已处理
	}
	if rec.ApprovalID != ref.ID || rec.ObjectVersion != ref.ObjectVersion || rec.SimulationID != ref.Evidence || rec.BaselineVersion != ref.BaselineVersion ||
		rec.BaselineGeneration != ref.BaselineGeneration {
		return fmt.Errorf("customer-context：审批事件 %s 的绑定与本地申请不一致", env.ID)
	}
	effective := e.EffectiveAt.UTC()
	finish := func(state, reason string) error {
		rec.State, rec.DecidedBy = state, e.DecidedBy
		var r *string
		if reason != "" {
			rec.Reason, r = reason, &reason
		}
		if fresh {
			if _, err := tx.Exec(ctx, `INSERT INTO ctx_policy_requests (approval_id, tenant_id, objective_id, kind, state, object_version, simulation_id, baseline_version, baseline_generation, backfill_days, requested_by, decided_by, reason)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, rec.ApprovalID, env.Tenant, o.ID, rec.Kind, state, rec.ObjectVersion, rec.SimulationID,
				rec.BaselineVersion, rec.BaselineGeneration, rec.BackfillDays, rec.RequestedBy, e.DecidedBy, r); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE ctx_policy_requests SET state = $2, decided_by = $3, reason = $4, updated_at = now() WHERE approval_id = $1`,
			rec.ApprovalID, state, e.DecidedBy, r); err != nil {
			return err
		}
		if p.locked() == rec.ApprovalID && state != "executed" {
			if _, err := tx.Exec(ctx, `UPDATE ctx_policies SET draft_locked_by = NULL, updated_at = now() WHERE objective_id = $1`, o.ID); err != nil {
				return err
			}
			p.lockedBy = nil
		}
		return nil
	}
	if e.Decision != "approve" {
		if err := finish("rejected", e.Reason); err != nil {
			return err
		}
		return recorder.Record(ctx, tx, audit.Entry{Action: "customer_context.policy." + rec.Kind + "_rejected", Resource: fmt.Sprintf("%s/%s", o.ID, rec.ApprovalID),
			Result: "succeeded", Basis: "identity.approval.decided"})
	}
	mismatch, err := checkBinding(ctx, tx, o, p, *ref)
	if err != nil {
		return err
	}
	if mismatch == "" && rec.RequestedBy == e.DecidedBy {
		mismatch = "object_changed" // 四眼由 identity 保证；不应发生，不执行
	}
	if mismatch != "" {
		if err := finish("failed", mismatch); err != nil {
			return err
		}
		return emit(ctx, tx, PolicyExecutionFailed, o.ID, map[string]any{"objective_id": o.ID, "approval_id": rec.ApprovalID, "kind": rec.Kind,
			"reason": mismatch, "effective_at": effective.Format(time.RFC3339Nano)},
			"customer_context.policy.execution_failed", fmt.Sprintf("%s/%s", o.ID, rec.ApprovalID), "identity.approval.decided")
	}
	if err := finish("executed", ""); err != nil {
		return err
	}
	if rec.Kind == kindActivation {
		return s.activate(ctx, tx, env.Tenant, o, p, rec, e.DecidedBy, effective)
	}
	return s.rollback(ctx, tx, env.Tenant, o, p, rec, effective)
}

// switchActive 把生效版本切换为 version：原生效版本记为 superseded，策略代次加一，写入生效边界记录；目标未激活时置为生效。
func switchActive(ctx context.Context, tx pgx.Tx, tenant string, o *Objective, p *policyRow, version int64, rec *RequestRec, effective time.Time) error {
	if p.active > 0 {
		if _, err := tx.Exec(ctx, `UPDATE ctx_policy_versions SET state = 'superseded' WHERE objective_id = $1 AND version = $2`, o.ID, p.active); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE ctx_policy_versions SET state = 'active' WHERE objective_id = $1 AND version = $2`, o.ID, version); err != nil {
		return err
	}
	previous := p.active
	p.generation, p.active = p.generation+1, version
	if version > p.latest {
		p.latest = version
	}
	if _, err := tx.Exec(ctx, `UPDATE ctx_policies SET generation = $2, active_version = $3, latest_version = $4, updated_at = now() WHERE objective_id = $1`,
		o.ID, p.generation, p.active, p.latest); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ctx_policy_activations (tenant_id, objective_id, generation, version, previous_version, kind, approval_id, effective_at)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, tenant, o.ID, p.generation, version, previous, rec.Kind, rec.ApprovalID, effective); err != nil {
		return err
	}
	if o.State == domain.StateInactive {
		o.State, o.Version = domain.StateActive, o.Version+1
		if _, err := tx.Exec(ctx, `UPDATE ctx_objectives SET state = $2, version = $3, updated_at = now() WHERE id = $1`, o.ID, o.State, o.Version); err != nil {
			return err
		}
		if err := emitObjective(ctx, tx, o, "activated", "can_approve_activation"); err != nil {
			return err
		}
	}
	return nil
}

// activate 把绑定的草案修订写为新的策略版本并使其生效，清除草案（调用方已核验绑定）。
func (s *Service) activate(ctx context.Context, tx pgx.Tx, tenant string, o *Objective, p *policyRow, rec *RequestRec, approver string, effective time.Time) error {
	r, err := readRevision(ctx, tx, o.ID, rec.ObjectVersion)
	if err != nil {
		return err
	}
	var domains []string
	if err := tx.QueryRow(ctx, `SELECT enabled_domains FROM ctx_policy_simulations WHERE simulation_id = $1`, rec.SimulationID).Scan(&domains); err != nil {
		return err
	}
	form, err := json.Marshal(r.form)
	if err != nil {
		return err
	}
	rules, err := json.Marshal(r.rules)
	if err != nil {
		return err
	}
	previous, version := p.active, p.latest+1
	if _, err := tx.Exec(ctx, `INSERT INTO ctx_policy_versions (tenant_id, objective_id, version, state, source_revision, template_id, template_version, form, rules, document, enabled_domains, simulation_id, approval_id, requested_by, approved_by, activated_at)
 VALUES ($1, $2, $3, 'superseded', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`, tenant, o.ID, version, r.revision, r.templateID, r.templateVersion, form, rules,
		r.document, domains, rec.SimulationID, rec.ApprovalID, rec.RequestedBy, approver, effective); err != nil {
		return err
	}
	if err := switchActive(ctx, tx, tenant, o, p, version, rec, effective); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE ctx_policies SET draft_revision = NULL, draft_version = draft_version + 1, draft_locked_by = NULL, updated_at = now() WHERE objective_id = $1`, o.ID); err != nil {
		return err
	}
	backfill := 0
	if rec.BackfillDays != nil && previous == 0 {
		backfill = *rec.BackfillDays
	}
	return emit(ctx, tx, PolicyActivated, o.ID, map[string]any{"objective_id": o.ID, "version": version, "generation": p.generation, "previous_version": previous,
		"approval_id": rec.ApprovalID, "source_revision": r.revision, "template_id": r.templateID, "template_version": r.templateVersion,
		"enabled_domains": domains, "backfill_days": backfill, "effective_at": effective.Format(time.RFC3339Nano)},
		"customer_context.policy.activated", fmt.Sprintf("%s/v%d", o.ID, version), "can_approve_activation")
}

// rollback 使目标版本重新生效（原合成文档与启用领域集合不变；调用方已核验绑定）。草案不受影响，其仿真随代次变化而过期。
func (s *Service) rollback(ctx context.Context, tx pgx.Tx, tenant string, o *Objective, p *policyRow, rec *RequestRec, effective time.Time) error {
	previous := p.active
	if err := switchActive(ctx, tx, tenant, o, p, rec.ObjectVersion, rec, effective); err != nil {
		return err
	}
	return emit(ctx, tx, PolicyRolledBack, o.ID, map[string]any{"objective_id": o.ID, "version": rec.ObjectVersion, "generation": p.generation,
		"previous_version": previous, "approval_id": rec.ApprovalID, "effective_at": effective.Format(time.RFC3339Nano)},
		"customer_context.policy.rolled_back", fmt.Sprintf("%s/v%d", o.ID, rec.ObjectVersion), "can_approve_activation")
}

// ApprovalClosed 是 identity.approval.closed.v1 的消费处理：待审批的申请记为 withdrawn / expired / invalidated，激活申请的草案解锁。
func (s *Service) ApprovalClosed(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	e, ref, err := parseApprovalEvent(env)
	if err != nil || ref == nil {
		return err
	}
	if e.State != "withdrawn" && e.State != "expired" && e.State != "invalidated" {
		return fmt.Errorf("customer-context：审批关闭事件 %s 的状态 %s 无效", env.ID, e.State)
	}
	p, err := lockPolicy(ctx, tx, env.Tenant, ref.Objective)
	if err != nil {
		return err
	}
	rec, err := readRequest(ctx, tx, ref.ID, true)
	if err != nil || rec == nil || rec.State != "pending" {
		return err // 未登记（对账时已不成立）或已处理
	}
	if _, err := tx.Exec(ctx, `UPDATE ctx_policy_requests SET state = $2, reason = $3, updated_at = now() WHERE approval_id = $1`, rec.ApprovalID, e.State, e.Reason); err != nil {
		return err
	}
	if p.locked() == rec.ApprovalID {
		if _, err := tx.Exec(ctx, `UPDATE ctx_policies SET draft_locked_by = NULL, updated_at = now() WHERE objective_id = $1`, ref.Objective); err != nil {
			return err
		}
	}
	return recorder.Record(ctx, tx, audit.Entry{Action: "customer_context.policy." + rec.Kind + "_" + e.State, Resource: fmt.Sprintf("%s/%s", ref.Objective, rec.ApprovalID),
		Result: "succeeded", Basis: "identity.approval.closed"})
}
