package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	identityapi "github.com/superailabs/miaoqia/services/contracts/apis/identity"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 审批（PRD 第7段 F4、F5；identity.md 第5节“审批”与 7.3）。
//
//	待审批 ──批准──► 已批准
//	   ├──驳回──► 已驳回
//	   ├──撤回（申请人）──► 已撤回
//	   ├──到期（7天）──► 已过期
//	   └──失效──► 已失效（申请人退出或失去申请关系；所属子系统报告绑定的对象版本或基准已不成立）
//
// 状态变化都在请求行锁下执行并使版本加一。到期按业务时钟（Directory.Now）判断：到期的待审批请求在被读取或操作前先在
// 独立事务中置为已过期（列表、读取、决定、撤回、提交前，以及 ExpireDueApprovals）。
//
// 锁顺序（避免死锁）：成员行 → 资源登记行 → 审批请求行。决定先对申请人的成员行加共享锁、对资源登记行加共享锁，再锁请求行，
// 并在锁内重核申请人仍有申请关系；成员命令（writeMember，成员行排他锁）与参与关系同步（资源登记行排他锁）在各自事务中
// 对受影响的待审批请求加锁并使其失效。因此“申请人退出或失权”与“批准”不会交错成“失权后仍批准”。
const (
	// ApprovalTTL 是审批请求的最长有效期（PRD 第7段第13节第3项）。
	ApprovalTTL = 7 * 24 * time.Hour
	// maxListedApprovals 是审批列表每页的默认与最大条数。
	maxListedApprovals = 100
	// approvalScanLimit 是审批人列表每次请求最多检查的请求数（逐个资源实时判断批准关系）；达到时返回已找到的部分并给出游标。
	approvalScanLimit = 500
)

// approvalKind 是一个已登记的审批类型。
type approvalKind struct {
	resource string // 资源类型
	request  string // 申请关系
	approve  string // 批准关系
	owner    string // 所属子系统（CloseApproval 的唯一调用方）
}

var approvalKinds = map[string]approvalKind{
	"policy_activation": {resource: "objective", request: "can_request_activation", approve: "can_approve_activation", owner: "customer-context"},
	"policy_rollback":   {resource: "objective", request: "can_request_activation", approve: "can_approve_activation", owner: "customer-context"},
}

type approval struct {
	id, tenant, kind, resource, applicant, note string
	binding                                     identityapi.ApprovalBinding
	state                                       string
	version                                     int64
	submittedAt, expiresAt                      time.Time
	decidedBy, decisionReason, closedReason     *string
	decidedAt, closedAt                         *time.Time
	createdAt                                   time.Time
}

const approvalColumns = `id,tenant_id,type,resource,applicant,note,binding,state,version,submitted_at,expires_at,decided_by,decided_at,decision_reason,closed_reason,closed_at,created_at`

func scanApproval(row pgx.Row) (*approval, error) {
	var a approval
	var binding []byte
	if err := row.Scan(&a.id, &a.tenant, &a.kind, &a.resource, &a.applicant, &a.note, &binding, &a.state, &a.version, &a.submittedAt, &a.expiresAt,
		&a.decidedBy, &a.decidedAt, &a.decisionReason, &a.closedReason, &a.closedAt, &a.createdAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(binding, &a.binding); err != nil {
		return nil, err
	}
	return &a, nil
}

// readApproval 读取本租户的请求。其他租户的与不存在的请求返回同一 404 approval_not_found。
func readApproval(ctx context.Context, tx pgx.Tx, tenantID, id string, lock bool) (*approval, error) {
	q := `SELECT ` + approvalColumns + ` FROM identity_approvals WHERE tenant_id=$1 AND id=$2`
	if lock {
		q += " FOR UPDATE"
	}
	a, err := scanApproval(tx.QueryRow(ctx, q, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(404, "approval_not_found")
	}
	return a, err
}

func (a *approval) due(now time.Time) bool { return a.state == "pending" && !now.Before(a.expiresAt) }

func (a *approval) out() *identityapi.ApprovalRequest {
	o := &identityapi.ApprovalRequest{ID: identityapi.ApprovalID(a.id), Type: identityapi.ApprovalType(a.kind), Resource: a.resource, Applicant: a.applicant,
		Note: a.note, Binding: a.binding, State: identityapi.ApprovalState(a.state), Version: a.version, SubmittedAt: a.submittedAt.UTC(), ExpiresAt: a.expiresAt.UTC()}
	if a.decidedBy != nil {
		o.DecidedBy = identityapi.NewOptString(*a.decidedBy)
	}
	if a.decidedAt != nil {
		o.DecidedAt = identityapi.NewOptDateTime(a.decidedAt.UTC())
	}
	if a.decisionReason != nil {
		o.DecisionReason = identityapi.NewOptString(*a.decisionReason)
	}
	if a.closedReason != nil {
		o.ClosedReason = identityapi.NewOptApprovalRequestClosedReason(identityapi.ApprovalRequestClosedReason(*a.closedReason))
	}
	if a.closedAt != nil {
		o.ClosedAt = identityapi.NewOptDateTime(a.closedAt.UTC())
	}
	return o
}

// bindingData 是绑定的 JSON 形式（缺省的回填天数不出现）。生成类型的可选字段只能经指针编码，这里显式展开。
func bindingData(b identityapi.ApprovalBinding) map[string]any {
	out := map[string]any{"object_version": b.ObjectVersion, "evidence_ref": b.EvidenceRef, "baseline_version": b.BaselineVersion,
		"baseline_generation": b.BaselineGeneration}
	if d, ok := b.BackfillDays.Get(); ok {
		out["backfill_days"] = d
	}
	return out
}

// emitApproval 在事务中发布 identity.approval.<event>.v1 并写审计。按资源分区：同一目标的请求事件有序。
func emitApproval(ctx context.Context, tx pgx.Tx, a *approval, event string, effective time.Time, extra map[string]any, basis string) error {
	data := map[string]any{"tenant_id": a.tenant, "approval_id": a.id, "type": a.kind, "resource": a.resource, "applicant": a.applicant,
		"version": a.version, "binding": bindingData(a.binding), "effective_at": effective.UTC().Format(time.RFC3339Nano)}
	for k, v := range extra {
		data[k] = v
	}
	w := outbox.Writer{Subsystem: api.Name, Namespace: "identity", Zone: reqctx.ZonePlatform}
	if _, err := w.Append(ctx, tx, outbox.Event{Type: "identity.approval." + event + ".v1", Subject: a.id, PartitionKey: a.resource, Data: data}); err != nil {
		return err
	}
	action := "identity.approval." + event
	if a.closedReason != nil {
		action += "." + *a.closedReason
	}
	return (audit.Recorder{Subsystem: api.Name}).Record(ctx, tx, audit.Entry{Action: action, Resource: a.id, Result: "succeeded", Basis: basis})
}

// closeLocked 未经决定关闭待审批的请求（调用方持有请求行锁）：withdrawn、expired 或 invalidated。
func closeLocked(ctx context.Context, tx pgx.Tx, a *approval, state, reason string, at time.Time, basis string) error {
	a.state, a.version, a.closedReason, a.closedAt = state, a.version+1, &reason, &at
	if _, err := tx.Exec(ctx, `UPDATE identity_approvals SET state=$2, version=$3, closed_reason=$4, closed_at=$5, updated_at=now() WHERE id=$1`,
		a.id, state, a.version, reason, at); err != nil {
		return err
	}
	return emitApproval(ctx, tx, a, "closed", at, map[string]any{"state": state, "reason": reason}, basis)
}

// expireDueApprovals 在事务中把本租户（可限定请求）到期的待审批请求置为已过期，返回处理数量。
func (d *Directory) expireDueApprovals(ctx context.Context, tx pgx.Tx, tenantID, id string) (int, error) {
	q := `SELECT ` + approvalColumns + ` FROM identity_approvals WHERE tenant_id=$1 AND state='pending' AND expires_at <= $2`
	args := []any{tenantID, d.now()}
	if id != "" {
		q += ` AND id=$3`
		args = append(args, id)
	}
	rows, err := tx.Query(ctx, q+` ORDER BY expires_at LIMIT 500 FOR UPDATE`, args...)
	if err != nil {
		return 0, err
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*approval, error) { return scanApproval(r) })
	if err != nil {
		return 0, err
	}
	for _, a := range due {
		if err := closeLocked(ctx, tx, a, "expired", "expired", a.expiresAt, "approval_expiry"); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// expireApprovalsBefore 在独立事务中先处理到期的请求，使随后的命令按已过期状态判断（命令失败回滚时过期仍保留）。
func (d *Directory) expireApprovalsBefore(ctx context.Context, tenantID, id string) error {
	return d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for {
			n, err := d.expireDueApprovals(ctx, tx, tenantID, id)
			if err != nil || n < 500 {
				return err
			}
		}
	})
}

// ExpireDueApprovals 把全部租户中到期的待审批请求置为已过期（供定时任务调用，以服务主体执行），返回处理数量。
func (d *Directory) ExpireDueApprovals(ctx context.Context, limit int) (int, error) {
	type item struct{ tenant, cell string }
	var tenants []item
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT a.tenant_id, t.cell FROM identity_approvals a JOIN identity_tenants t ON t.id=a.tenant_id
 WHERE a.state='pending' AND a.expires_at <= $1 LIMIT $2`, d.now(), limit)
		if err != nil {
			return err
		}
		tenants, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
			var it item
			return it, r.Scan(&it.tenant, &it.cell)
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for _, it := range tenants {
		sctx := reqctx.With(ctx, reqctx.Context{Tenant: it.tenant, Cell: it.cell, Principal: reqctx.Principal{ID: "svc:" + api.Name, Kind: "service"}})
		err := d.tx(sctx, func(ctx context.Context, tx pgx.Tx) error {
			n, err := d.expireDueApprovals(ctx, tx, it.tenant, "")
			total += n
			return err
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// relationOn 以当前目录状态判断成员（会话）对资源的关系。资源不属于本租户或未登记时为 false。
func (d *Directory) relationOn(ctx context.Context, tx pgx.Tx, s *api.Session, relation, object string) (bool, error) {
	t, err := resolveTarget(ctx, tx, s.Tenant, object)
	if err != nil || t == nil {
		return false, err
	}
	allowed, err := d.evaluate(ctx, s, relation, t)
	if err != nil {
		return false, failure(503, "identity_unavailable")
	}
	return allowed, nil
}

// applicantStanding 重核申请人此刻是否仍是有效成员并有申请关系：返回空串表示仍成立，否则为失效原因
// （applicant_left、applicant_lost_permission）。授权服务不可用时返回错误（未知不当作失权）。
func (d *Directory) applicantStanding(ctx context.Context, tx pgx.Tx, a *approval, cell string) (string, error) {
	s, err := readMember(ctx, tx, a.tenant, a.applicant, cell, false)
	var safe *api.Error
	if errors.As(err, &safe) && safe.Code == "member_not_found" {
		return "applicant_left", nil
	}
	if err != nil {
		return "", err
	}
	if s.State != "active" {
		return "applicant_left", nil
	}
	ok, err := d.relationOn(ctx, tx, s, approvalKinds[a.kind].request, a.resource)
	if err != nil {
		return "", err
	}
	if !ok {
		return "applicant_lost_permission", nil
	}
	return "", nil
}

// invalidatePending 使给定条件下的待审批请求失效：申请人退出（subject 非空、resource 为空）时全部失效；
// 否则逐个重核申请人的申请关系，只使失权的请求失效。调用方已持有相关成员行或资源登记行的排他锁。
func (d *Directory) invalidatePending(ctx context.Context, tx pgx.Tx, tenantID, cell, subject, resource string, left bool) error {
	q := `SELECT ` + approvalColumns + ` FROM identity_approvals WHERE tenant_id=$1 AND state='pending'`
	args := []any{tenantID}
	if subject != "" {
		args = append(args, subject)
		q += ` AND applicant=$` + strconv.Itoa(len(args))
	}
	if resource != "" {
		args = append(args, resource)
		q += ` AND resource=$` + strconv.Itoa(len(args))
	}
	rows, err := tx.Query(ctx, q+` ORDER BY id FOR UPDATE`, args...)
	if err != nil {
		return err
	}
	pending, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*approval, error) { return scanApproval(r) })
	if err != nil {
		return err
	}
	now := d.businessNow()
	for _, a := range pending {
		reason := "applicant_left"
		if !left {
			if reason, err = d.applicantStanding(ctx, tx, a, cell); err != nil {
				return err
			}
			if reason == "" {
				continue
			}
		}
		if err := closeLocked(ctx, tx, a, "invalidated", reason, now, "approval_applicant_check"); err != nil {
			return err
		}
	}
	return nil
}

func validBinding(kind string, b identityapi.ApprovalBinding) bool {
	if b.Validate() != nil {
		return false
	}
	_, backfill := b.BackfillDays.Get()
	switch kind {
	case "policy_activation":
		// 回填只用于首次激活（尚无生效版本）
		return !backfill || b.BaselineVersion == 0
	case "policy_rollback":
		return !backfill && b.BaselineVersion >= 1 && b.ObjectVersion != b.BaselineVersion
	}
	return false
}

// RequestApproval 是 api.Approvals：以请求上下文的用户为申请人提交审批请求（契约 requestApproval）。
func (d *Directory) RequestApproval(ctx context.Context, req *identityapi.RequestApprovalRequest, key string) (*identityapi.ApprovalRequest, error) {
	if req == nil || req.Validate() != nil || strings.TrimSpace(req.Note) == "" {
		return nil, failure(400, "invalid_request")
	}
	kind, ok := approvalKinds[string(req.Type)]
	if !ok || !strings.HasPrefix(req.Resource, kind.resource+":") || !validBinding(string(req.Type), req.Binding) {
		return nil, failure(400, "invalid_request")
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	allowed, err := d.CheckMember(ctx, kind.request, req.Resource)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, failure(403, "forbidden")
	}
	if err := d.strongAuth(ctx, rc.Principal.ID, nil); err != nil {
		return nil, err
	}
	if err := d.expireApprovalsBefore(ctx, rc.Tenant, ""); err != nil {
		return nil, err
	}
	out := &identityapi.ApprovalRequest{}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "approval.request", req, out, func() error {
			// 申请人成员行共享锁：与成员命令（退出、角色变更）串行；之后的退出会看到本请求并使其失效
			s, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT 1 FROM identity_members WHERE tenant_id=$1 AND subject=$2 FOR SHARE`, rc.Tenant, rc.Principal.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT 1 FROM identity_resources WHERE object=$1 FOR SHARE`, req.Resource); err != nil {
				return err
			}
			// 锁内重核申请关系（锁前的判断可能已被并发的成员或参与关系变更推翻）
			ok, err := d.relationOn(ctx, tx, s, kind.request, req.Resource)
			if err != nil {
				return err
			}
			if !ok {
				return failure(403, "forbidden")
			}
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "identity-approvals/"+req.Resource); err != nil {
				return err
			}
			var busy bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_approvals WHERE resource=$1 AND state='pending')`, req.Resource).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return failure(409, "approval_in_progress")
			}
			now := d.businessNow()
			a := &approval{id: ids.New("apr"), tenant: rc.Tenant, kind: string(req.Type), resource: req.Resource, applicant: rc.Principal.ID, note: req.Note,
				binding: req.Binding, state: "pending", version: 1, submittedAt: now, expiresAt: now.Add(ApprovalTTL)}
			encoded, err := json.Marshal(bindingData(a.binding))
			if err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `INSERT INTO identity_approvals(id,tenant_id,type,resource,applicant,note,binding,state,version,submitted_at,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,'pending',1,$8,$9) RETURNING created_at`, a.id, a.tenant, a.kind, a.resource, a.applicant, a.note, encoded, a.submittedAt, a.expiresAt).Scan(&a.createdAt); err != nil {
				if uniqueViolation(err) {
					return failure(409, "approval_in_progress")
				}
				return err
			}
			if err := emitApproval(ctx, tx, a, "requested", now, map[string]any{"expires_at": a.expiresAt.UTC().Format(time.RFC3339Nano)}, kind.request); err != nil {
				return err
			}
			*out = *a.out()
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// visibility 判断调用者对请求的可见性：申请人本人，或此刻对请求资源有批准关系的成员。
func (d *Directory) approverOf(ctx context.Context, tx pgx.Tx, s *api.Session, a *approval) (bool, error) {
	return d.relationOn(ctx, tx, s, approvalKinds[a.kind].approve, a.resource)
}

// GetApproval 读取审批请求（契约 getApproval）。
func (d *Directory) GetApproval(ctx context.Context, id string) (*identityapi.ApprovalRequest, error) {
	if identityapi.ApprovalID(id).Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	if err := d.expireApprovalsBefore(ctx, rc.Tenant, id); err != nil {
		return nil, err
	}
	var out *identityapi.ApprovalRequest
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		if err != nil {
			return err
		}
		a, err := readApproval(ctx, tx, rc.Tenant, id, false)
		if err != nil {
			return err
		}
		if a.applicant != rc.Principal.ID {
			ok, err := d.approverOf(ctx, tx, s, a)
			if err != nil {
				return err
			}
			if !ok {
				return failure(404, "approval_not_found")
			}
		}
		out = a.out()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DecideApproval 批准或驳回（契约 decideApproval，核验顺序见契约）。
func (d *Directory) DecideApproval(ctx context.Context, id string, req *identityapi.DecideApprovalRequest, key string) (*identityapi.ApprovalRequest, error) {
	if req == nil || req.Validate() != nil || identityapi.ApprovalID(id).Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	reason, hasReason := req.Reason.Get()
	if hasReason && strings.TrimSpace(reason) == "" {
		return nil, failure(400, "invalid_request")
	}
	if req.Decision == identityapi.DecideApprovalRequestDecisionReject && !hasReason {
		return nil, failure(400, "reason_required")
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	// 先授权、再核对强认证，均在返回已存回执之前
	var applicant, resource string
	if err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		if err != nil {
			return err
		}
		a, err := readApproval(ctx, tx, rc.Tenant, id, false)
		if err != nil {
			return err
		}
		applicant, resource = a.applicant, a.resource
		if a.applicant == rc.Principal.ID {
			return failure(403, "self_approval_forbidden")
		}
		ok, err := d.approverOf(ctx, tx, s, a)
		if err != nil {
			return err
		}
		if !ok {
			return failure(404, "approval_not_found")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := d.strongAuth(ctx, rc.Principal.ID, nil); err != nil {
		return nil, err
	}
	if err := d.expireApprovalsBefore(ctx, rc.Tenant, id); err != nil {
		return nil, err
	}
	out := &identityapi.ApprovalRequest{}
	invalidated := false
	request := struct {
		ID  string                             `json:"id"`
		Req *identityapi.DecideApprovalRequest `json:"request"`
	}{id, req}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "approval.decide", request, out, func() error {
			// 锁顺序：申请人与决定人的成员行（共享）→ 资源登记行（共享）→ 请求行
			if _, err := tx.Exec(ctx, `SELECT 1 FROM identity_members WHERE tenant_id=$1 AND subject = ANY($2) ORDER BY subject FOR SHARE`, rc.Tenant, []string{applicant, rc.Principal.ID}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT 1 FROM identity_resources WHERE object=$1 FOR SHARE`, resource); err != nil {
				return err
			}
			a, err := readApproval(ctx, tx, rc.Tenant, id, true)
			if err != nil {
				return err
			}
			// 锁内重核决定人的批准关系
			s, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
			if err != nil {
				return err
			}
			ok, err := d.approverOf(ctx, tx, s, a)
			if err != nil {
				return err
			}
			if !ok {
				return failure(404, "approval_not_found")
			}
			if a.state != "pending" || a.due(d.now()) {
				return failure(409, "approval_not_pending")
			}
			if a.version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			why, err := d.applicantStanding(ctx, tx, a, rc.Cell)
			if err != nil {
				return err
			}
			if why != "" {
				// 失效在本事务中提交，本次决定不作出（返回 409 approval_invalidated，不记录回执）
				invalidated = true
				return closeLocked(ctx, tx, a, "invalidated", why, d.businessNow(), "approval_applicant_check")
			}
			now := d.businessNow()
			state := "approved"
			if req.Decision == identityapi.DecideApprovalRequestDecisionReject {
				state = "rejected"
			}
			decider := rc.Principal.ID
			a.state, a.version, a.decidedBy, a.decidedAt = state, a.version+1, &decider, &now
			if hasReason {
				a.decisionReason = &reason
			}
			if _, err := tx.Exec(ctx, `UPDATE identity_approvals SET state=$2, version=$3, decided_by=$4, decided_at=$5, decision_reason=$6, updated_at=now() WHERE id=$1`,
				a.id, a.state, a.version, decider, now, a.decisionReason); err != nil {
				return err
			}
			extra := map[string]any{"decision": string(req.Decision), "decided_by": decider}
			if hasReason {
				extra["reason"] = reason
			}
			if err := emitApproval(ctx, tx, a, "decided", now, extra, approvalKinds[a.kind].approve); err != nil {
				return err
			}
			*out = *a.out()
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if invalidated {
		return nil, failure(409, "approval_invalidated")
	}
	return out, nil
}

// WithdrawApproval 申请人在决定前撤回（契约 withdrawApproval）。
func (d *Directory) WithdrawApproval(ctx context.Context, id string, req *identityapi.ApprovalVersionRequest, key string) (*identityapi.ApprovalRequest, error) {
	if req == nil || req.Validate() != nil || identityapi.ApprovalID(id).Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	if err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		if err != nil {
			return err
		}
		a, err := readApproval(ctx, tx, rc.Tenant, id, false)
		if err != nil {
			return err
		}
		if a.applicant == rc.Principal.ID {
			return nil
		}
		ok, err := d.approverOf(ctx, tx, s, a)
		if err != nil {
			return err
		}
		if ok {
			return failure(403, "forbidden")
		}
		return failure(404, "approval_not_found")
	}); err != nil {
		return nil, err
	}
	if err := d.expireApprovalsBefore(ctx, rc.Tenant, id); err != nil {
		return nil, err
	}
	out := &identityapi.ApprovalRequest{}
	request := struct {
		ID  string                              `json:"id"`
		Req *identityapi.ApprovalVersionRequest `json:"request"`
	}{id, req}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "approval.withdraw", request, out, func() error {
			if _, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell); err != nil {
				return err
			}
			a, err := readApproval(ctx, tx, rc.Tenant, id, true)
			if err != nil {
				return err
			}
			if a.state != "pending" || a.due(d.now()) {
				return failure(409, "approval_not_pending")
			}
			if a.version != req.ExpectedVersion {
				return failure(409, "version_conflict")
			}
			if err := closeLocked(ctx, tx, a, "withdrawn", "withdrawn", d.businessNow(), "applicant"); err != nil {
				return err
			}
			*out = *a.out()
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CloseApproval 是 api.Approvals：所属子系统使绑定已不成立的待审批请求失效（契约 closeApproval）。
func (d *Directory) CloseApproval(ctx context.Context, id string, req *identityapi.CloseApprovalRequest) (*identityapi.ApprovalRequest, error) {
	if req == nil || req.Validate() != nil || identityapi.ApprovalID(id).Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Principal.Kind != "service" || rc.Tenant == "" {
		return nil, failure(403, "forbidden")
	}
	var out *identityapi.ApprovalRequest
	err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := serviceTenant(ctx, tx, rc); err != nil {
			return err
		}
		a, err := readApproval(ctx, tx, rc.Tenant, id, true)
		if err != nil {
			return err
		}
		if rc.Principal.ID != "svc:"+approvalKinds[a.kind].owner {
			return failure(403, "forbidden")
		}
		if a.due(d.now()) {
			if err := closeLocked(ctx, tx, a, "expired", "expired", a.expiresAt, "approval_expiry"); err != nil {
				return err
			}
		}
		if a.state == "pending" {
			if err := closeLocked(ctx, tx, a, "invalidated", string(req.Reason), d.businessNow(), approvalKinds[a.kind].owner); err != nil {
				return err
			}
		}
		out = a.out()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// approvalCursor 是审批列表的翻页位置：上一页最后检查的 (created_at, id) 与签发它的租户、调用者、role 与 state 的摘要。
type approvalCursor struct {
	CreatedAt int64  `json:"t"`
	ID        string `json:"i"`
	Filter    string `json:"f"`
}

func approvalFilter(tenantID, subject, role, state string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + subject + "\x00" + role + "\x00" + state))
	return hex.EncodeToString(sum[:8])
}

// ListApprovals 分页列出审批请求（契约 listApprovals）。审批人视角逐个资源实时判断批准关系（同一资源只判断一次）；
// 一次至多检查 approvalScanLimit 条，达到时返回已找到的部分（可能少于 limit）并给出游标。
func (d *Directory) ListApprovals(ctx context.Context, params identityapi.ListApprovalsParams) (*identityapi.ApprovalList, error) {
	if l, ok := params.Limit.Get(); ok && (l < 1 || l > maxListedApprovals) {
		return nil, failure(400, "invalid_request")
	}
	if s, ok := params.State.Get(); ok && s.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	if r, ok := params.Role.Get(); ok && r.Validate() != nil {
		return nil, failure(400, "invalid_request")
	}
	rc, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Tenant == "" {
		return nil, failure(403, "invalid_tenant_context")
	}
	role := string(params.Role.Or(identityapi.ListApprovalsRoleApprover))
	state := ""
	if s, ok := params.State.Get(); ok {
		state = string(s)
	} else if role == "approver" {
		state = "pending"
	}
	limit := params.Limit.Or(maxListedApprovals)
	filter := approvalFilter(rc.Tenant, rc.Principal.ID, role, state)
	var after *approvalCursor
	if c, ok := params.Cursor.Get(); ok {
		raw, err := base64.RawURLEncoding.DecodeString(string(c))
		var v approvalCursor
		if err != nil || json.Unmarshal(raw, &v) != nil || identityapi.ApprovalID(v.ID).Validate() != nil || v.Filter != filter {
			return nil, failure(400, "invalid_cursor")
		}
		after = &v
	}
	if err := d.expireApprovalsBefore(ctx, rc.Tenant, ""); err != nil {
		return nil, err
	}
	out := &identityapi.ApprovalList{Approvals: []identityapi.ApprovalRequest{}}
	err = d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		if err != nil {
			return err
		}
		q := `SELECT ` + approvalColumns + ` FROM identity_approvals WHERE tenant_id=$1`
		args := []any{rc.Tenant, rc.Principal.ID}
		if role == "applicant" {
			q += ` AND applicant=$2`
		} else {
			q += ` AND applicant<>$2`
		}
		if state != "" {
			args = append(args, state)
			q += ` AND state=$` + strconv.Itoa(len(args))
		}
		if after != nil {
			args = append(args, time.UnixMicro(after.CreatedAt).UTC(), after.ID)
			q += ` AND (created_at, id) < ($` + strconv.Itoa(len(args)-1) + `, $` + strconv.Itoa(len(args)) + `)`
		}
		scan := limit + 1
		if role == "approver" {
			scan = approvalScanLimit + 1
		}
		rows, err := tx.Query(ctx, q+` ORDER BY created_at DESC, id DESC LIMIT `+strconv.Itoa(scan), args...)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*approval, error) { return scanApproval(r) })
		if err != nil {
			return err
		}
		allowed := map[string]bool{}
		var last *approval
		for i, a := range list {
			if role == "approver" && i == approvalScanLimit {
				// 检查上限：已找到的部分加游标（游标指向最后检查的一条）
				out.Truncated = true
				break
			}
			if len(out.Approvals) == limit {
				out.Truncated = true
				break
			}
			if role == "approver" {
				ok, seen := allowed[a.resource]
				if !seen {
					if ok, err = d.approverOf(ctx, tx, s, a); err != nil {
						return err
					}
					allowed[a.resource] = ok
				}
				last = a
				if !ok {
					continue
				}
			}
			last = a
			out.Approvals = append(out.Approvals, *a.out())
		}
		if out.Truncated && last != nil {
			raw, _ := json.Marshal(approvalCursor{CreatedAt: last.createdAt.UnixMicro(), ID: last.id, Filter: filter})
			out.NextCursor = identityapi.NewOptApprovalCursor(identityapi.ApprovalCursor(base64.RawURLEncoding.EncodeToString(raw)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
