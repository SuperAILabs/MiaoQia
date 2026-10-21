package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// 策略（PRD 第7段；customer-context.md 第4、6节）。每个目标一份策略状态（ctx_policies）：策略代次、生效版本与草案指针。
//
//	草案：每次保存形成不可变的新修订并发布 policy.drafted（learning 自动仿真）；放弃只清除草案指针。
//	      提交激活申请后锁定（draft_locked_by），撤回、驳回、失效或执行（成功或失败）后解锁。
//	版本：只由批准后的执行写入；激活与回滚在同一事务中改变生效版本并递增策略代次（生效边界，PRD 第5节）。
//
// 并发：策略状态行在命令事务中加排他锁（FOR UPDATE），同一目标的保存、放弃、申请登记与执行串行；草案带乐观并发版本号
// draft_version（每次保存或放弃加一），expected_version 不一致时拒绝，不覆盖他人修改。
const (
	PolicyDrafted         = "customer_context.policy.drafted.v1"
	PolicyActivated       = "customer_context.policy.activated.v1"
	PolicyRolledBack      = "customer_context.policy.rolled_back.v1"
	PolicyExecutionFailed = "customer_context.policy.execution_failed.v1"
	// maxListedVersions 是版本历史每页的默认与最大条数。
	maxListedVersions = 100
)

// SimulationRec 是附在草案修订上的仿真运行。
type SimulationRec struct {
	ID                string    `json:"simulation_id"`
	Status            string    `json:"status"`
	FailureReason     string    `json:"failure_reason,omitempty"`
	Stale             bool      `json:"stale"`
	CompareVersion    int64     `json:"compare_version"`
	CompareGeneration int64     `json:"compare_generation"`
	Domains           []string  `json:"enabled_domains"`
	CompletedAt       time.Time `json:"completed_at"`
}

// DraftRec 是当前草案（最新修订）。
type DraftRec struct {
	Revision        int64             `json:"revision"`
	BaseVersion     int64             `json:"base_version"`
	TemplateID      string            `json:"template_id"`
	TemplateVersion string            `json:"template_version"`
	Form            PolicyForm        `json:"form"`
	Rules           []RuleView        `json:"rules"`
	Valid           bool              `json:"valid"`
	Issues          []Issue           `json:"issues"`
	Mismatches      []ExampleMismatch `json:"example_mismatches"`
	SavedBy         string            `json:"saved_by"`
	SavedAt         time.Time         `json:"saved_at"`
	LockedBy        string            `json:"locked_by,omitempty"`
	Simulation      *SimulationRec    `json:"simulation,omitempty"`
}

// VersionRec 是策略版本。
type VersionRec struct {
	Version         int64      `json:"version"`
	State           string     `json:"state"`
	TemplateID      string     `json:"template_id"`
	TemplateVersion string     `json:"template_version"`
	Form            PolicyForm `json:"form"`
	Rules           []RuleView `json:"rules"`
	Domains         []string   `json:"enabled_domains"`
	SimulationID    string     `json:"simulation_id"`
	ApprovalID      string     `json:"approval_id"`
	RequestedBy     string     `json:"requested_by"`
	ApprovedBy      string     `json:"approved_by"`
	ActivatedAt     time.Time  `json:"activated_at"`
	SourceRevision  int64      `json:"source_revision"`
}

// RequestRec 是激活或回滚申请的本地执行记录。
type RequestRec struct {
	ApprovalID         string `json:"approval_id"`
	Kind               string `json:"kind"`
	State              string `json:"state"`
	ObjectVersion      int64  `json:"object_version"`
	SimulationID       string `json:"simulation_id"`
	BaselineVersion    int64  `json:"baseline_version"`
	BaselineGeneration int64  `json:"baseline_generation"`
	BackfillDays       *int   `json:"backfill_days,omitempty"`
	RequestedBy        string `json:"requested_by"`
	DecidedBy          string `json:"decided_by,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

// PolicyState 是策略页。
type PolicyState struct {
	ObjectiveID   string      `json:"objective_id"`
	Generation    int64       `json:"generation"`
	ActiveVersion int64       `json:"active_version"`
	Active        *VersionRec `json:"active,omitempty"`
	DraftVersion  int64       `json:"draft_version"`
	Draft         *DraftRec   `json:"draft,omitempty"`
	Request       *RequestRec `json:"request,omitempty"`
}

// policyRow 是 ctx_policies 的一行（不存在时为零值：从未有过草案与版本）。
type policyRow struct {
	objective                             string
	generation, active, latest, latestRev int64
	draftRevision                         *int64
	draftVersion                          int64
	lockedBy                              *string
}

func readPolicyRow(ctx context.Context, tx pgx.Tx, objective string, lock bool) (*policyRow, error) {
	q := `SELECT objective_id, generation, active_version, latest_version, latest_revision, draft_revision, draft_version, draft_locked_by FROM ctx_policies WHERE objective_id = $1`
	if lock {
		q += " FOR UPDATE"
	}
	var p policyRow
	err := tx.QueryRow(ctx, q, objective).Scan(&p.objective, &p.generation, &p.active, &p.latest, &p.latestRev, &p.draftRevision, &p.draftVersion, &p.lockedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return &policyRow{objective: objective}, nil
	}
	return &p, err
}

// lockPolicy 建立（第2段已启用、尚无策略状态的目标）并锁定策略状态行。
func lockPolicy(ctx context.Context, tx pgx.Tx, tenant, objective string) (*policyRow, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO ctx_policies (objective_id, tenant_id, policy_id) VALUES ($1, $2, $3) ON CONFLICT (objective_id) DO NOTHING`,
		objective, tenant, ids.New("pol")); err != nil {
		return nil, err
	}
	return readPolicyRow(ctx, tx, objective, true)
}

func (p *policyRow) locked() string {
	if p.lockedBy == nil {
		return ""
	}
	return *p.lockedBy
}

func (s *Service) businessNow() time.Time {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return now().UTC().Truncate(time.Second)
}

// template 返回目标所用的完整策略模板与其参数定义；目录中的模板版本已不同于目标所用版本时为 409 template_version_changed
// （模板升级随 F7 实现）。
func (s *Service) policyTemplate(o *Objective) (*policy.Policy, domain.PolicyTemplate, error) {
	tpl, ok := s.Policies[o.PolicyTemplateID]
	dtpl, dok := s.Catalog.PolicyTemplates[o.PolicyTemplateID]
	if !ok || !dok || tpl.Version != o.PolicyTemplateVersion || dtpl.Version != o.PolicyTemplateVersion {
		return nil, domain.PolicyTemplate{}, failure(409, "template_version_changed")
	}
	return tpl, dtpl, nil
}

type revisionRow struct {
	revision, base              int64
	templateID, templateVersion string
	form                        PolicyForm
	rules                       []RuleView
	document                    []byte
	valid                       bool
	issues                      []Issue
	mismatches                  []ExampleMismatch
	savedBy                     string
	savedAt                     time.Time
}

func readRevision(ctx context.Context, tx pgx.Tx, objective string, revision int64) (*revisionRow, error) {
	var r revisionRow
	var form, rules, issues, mismatches []byte
	err := tx.QueryRow(ctx, `SELECT revision, base_version, template_id, template_version, form, rules, document, valid, issues, example_mismatches, saved_by, saved_at
 FROM ctx_policy_revisions WHERE objective_id = $1 AND revision = $2`, objective, revision).Scan(&r.revision, &r.base, &r.templateID, &r.templateVersion,
		&form, &rules, &r.document, &r.valid, &issues, &mismatches, &r.savedBy, &r.savedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(404, "policy_revision_not_found")
	}
	if err != nil {
		return nil, err
	}
	for _, x := range []struct {
		raw []byte
		v   any
	}{{form, &r.form}, {rules, &r.rules}, {issues, &r.issues}, {mismatches, &r.mismatches}} {
		if err := json.Unmarshal(x.raw, x.v); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

const versionColumns = `version, state, template_id, template_version, form, rules, enabled_domains, simulation_id, approval_id, requested_by, approved_by, activated_at, source_revision`

func scanVersion(row pgx.Row) (*VersionRec, error) {
	var v VersionRec
	var form, rules []byte
	if err := row.Scan(&v.Version, &v.State, &v.TemplateID, &v.TemplateVersion, &form, &rules, &v.Domains, &v.SimulationID, &v.ApprovalID,
		&v.RequestedBy, &v.ApprovedBy, &v.ActivatedAt, &v.SourceRevision); err != nil {
		return nil, err
	}
	v.ActivatedAt = v.ActivatedAt.UTC()
	if err := json.Unmarshal(form, &v.Form); err != nil {
		return nil, err
	}
	return &v, json.Unmarshal(rules, &v.Rules)
}

func readVersion(ctx context.Context, tx pgx.Tx, objective string, version int64) (*VersionRec, error) {
	v, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionColumns+` FROM ctx_policy_versions WHERE objective_id = $1 AND version = $2`, objective, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(404, "policy_version_not_found")
	}
	return v, err
}

// latestSimulation 是修订最近一次完成或失败的仿真（按完成时间、标识确定）。
func latestSimulation(ctx context.Context, tx pgx.Tx, objective string, revision int64) (*SimulationRec, error) {
	var sim SimulationRec
	var reason *string
	err := tx.QueryRow(ctx, `SELECT simulation_id, status, failure_reason, compare_version, compare_generation, enabled_domains, completed_at
 FROM ctx_policy_simulations WHERE objective_id = $1 AND revision = $2 ORDER BY completed_at DESC, simulation_id DESC LIMIT 1`, objective, revision).
		Scan(&sim.ID, &sim.Status, &reason, &sim.CompareVersion, &sim.CompareGeneration, &sim.Domains, &sim.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if reason != nil {
		sim.FailureReason = *reason
	}
	sim.CompletedAt = sim.CompletedAt.UTC()
	return &sim, err
}

const requestColumns = `approval_id, kind, state, object_version, simulation_id, baseline_version, baseline_generation, backfill_days, requested_by, decided_by, reason`

func scanRequest(row pgx.Row) (*RequestRec, error) {
	var r RequestRec
	var decided, reason *string
	if err := row.Scan(&r.ApprovalID, &r.Kind, &r.State, &r.ObjectVersion, &r.SimulationID, &r.BaselineVersion, &r.BaselineGeneration, &r.BackfillDays,
		&r.RequestedBy, &decided, &reason); err != nil {
		return nil, err
	}
	if decided != nil {
		r.DecidedBy = *decided
	}
	if reason != nil {
		r.Reason = *reason
	}
	return &r, nil
}

// readPolicyState 读取策略页。
func readPolicyState(ctx context.Context, tx pgx.Tx, objective string) (*PolicyState, error) {
	p, err := readPolicyRow(ctx, tx, objective, false)
	if err != nil {
		return nil, err
	}
	out := &PolicyState{ObjectiveID: objective, Generation: p.generation, ActiveVersion: p.active, DraftVersion: p.draftVersion}
	if p.active > 0 {
		if out.Active, err = readVersion(ctx, tx, objective, p.active); err != nil {
			return nil, err
		}
	}
	if p.draftRevision != nil {
		r, err := readRevision(ctx, tx, objective, *p.draftRevision)
		if err != nil {
			return nil, err
		}
		d := &DraftRec{Revision: r.revision, BaseVersion: r.base, TemplateID: r.templateID, TemplateVersion: r.templateVersion, Form: r.form, Rules: r.rules,
			Valid: r.valid, Issues: r.issues, Mismatches: r.mismatches, SavedBy: r.savedBy, SavedAt: r.savedAt.UTC(), LockedBy: p.locked()}
		if d.Simulation, err = latestSimulation(ctx, tx, objective, r.revision); err != nil {
			return nil, err
		}
		if d.Simulation != nil {
			// 报告固定对应一份修订与一个对比版本：生效版本或策略代次在仿真后变化即过期（草案再保存是新修订，没有附着的报告）
			d.Simulation.Stale = d.Simulation.CompareVersion != p.active || d.Simulation.CompareGeneration != p.generation
		}
		out.Draft = d
	}
	out.Request, err = scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM ctx_policy_requests WHERE objective_id = $1 ORDER BY created_at DESC, approval_id DESC LIMIT 1`, objective))
	if errors.Is(err, pgx.ErrNoRows) {
		out.Request, err = nil, nil
	}
	return out, err
}

// saveRevision 合成、编译、运行样例并写入新修订，使其成为当前草案，发布 policy.drafted。调用方持有策略状态行锁。
func (s *Service) saveRevision(ctx context.Context, tx pgx.Tx, tenant, actor string, o *Objective, p *policyRow, tpl *policy.Policy, form PolicyForm) error {
	doc, err := compose(tpl, form)
	if err != nil {
		return err
	}
	valid, issues, mismatches := validate(doc, s.Features)
	rules := ruleViews(tpl, form)
	document, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	encoded := map[string][]byte{}
	for name, v := range map[string]any{"form": form, "rules": rules, "issues": issues, "mismatches": mismatches} {
		if encoded[name], err = json.Marshal(v); err != nil {
			return err
		}
	}
	revision := p.latestRev + 1
	if _, err := tx.Exec(ctx, `INSERT INTO ctx_policy_revisions (tenant_id, objective_id, revision, base_version, template_id, template_version, form, rules, document, valid, issues, example_mismatches, saved_by, saved_at)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`, tenant, o.ID, revision, p.active, tpl.ID, tpl.Version, encoded["form"], encoded["rules"], document,
		valid, encoded["issues"], encoded["mismatches"], actor, s.businessNow()); err != nil {
		return err
	}
	p.latestRev, p.draftRevision, p.draftVersion = revision, &revision, p.draftVersion+1
	if _, err := tx.Exec(ctx, `UPDATE ctx_policies SET latest_revision = $2, draft_revision = $2, draft_version = $3, updated_at = now() WHERE objective_id = $1`,
		o.ID, revision, p.draftVersion); err != nil {
		return err
	}
	return emit(ctx, tx, PolicyDrafted, o.ID, map[string]any{"objective_id": o.ID, "revision": revision, "draft_version": p.draftVersion,
		"base_version": p.active, "generation": p.generation, "template_id": tpl.ID, "template_version": tpl.Version, "valid": valid},
		"customer_context.policy.drafted", fmt.Sprintf("%s/r%d", o.ID, revision), "can_draft_policy")
}

// createFirstDraft 在启用目标的同一事务中创建策略状态与首版草案（模板缺省 + 目标参数）。
func (s *Service) createFirstDraft(ctx context.Context, tx pgx.Tx, tenant, actor string, o *Objective) error {
	tpl, _, err := s.policyTemplate(o)
	if err != nil {
		return err
	}
	p, err := lockPolicy(ctx, tx, tenant, o.ID)
	if err != nil {
		return err
	}
	return s.saveRevision(ctx, tx, tenant, actor, o, p, tpl, initialForm(tpl, o.Params))
}

// readObjectiveShared 读取目标并加共享锁（与停用、激活等对目标行的排他写入串行）。
func readObjectiveShared(ctx context.Context, tx pgx.Tx, id string) (*Objective, error) {
	o, err := scanObjective(tx.QueryRow(ctx, `SELECT `+objectiveColumns+` FROM ctx_objectives WHERE id = $1 FOR SHARE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failure(404, "objective_not_found")
	}
	return o, err
}

// GetPolicy 读取策略页（can_view）。
func (s *Service) GetPolicy(ctx context.Context, id string) (*PolicyState, error) {
	if _, _, err := s.requireObjective(ctx, id, "can_view"); err != nil {
		return nil, err
	}
	var out *PolicyState
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = readPolicyState(ctx, tx, id)
		return err
	})
	return out, err
}

// DraftPolicyChange 保存草案（can_draft_policy）：整份表单形成新修订（契约 draftPolicyChange）。
func (s *Service) DraftPolicyChange(ctx context.Context, id string, expected int64, in PolicyForm, key string) (*PolicyState, error) {
	rc, _, err := s.requireObjective(ctx, id, "can_draft_policy")
	if err != nil {
		return nil, err
	}
	out := &PolicyState{}
	request := struct {
		ID       string     `json:"id"`
		Expected int64      `json:"expected_version"`
		Form     PolicyForm `json:"form"`
	}{id, expected, in}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "policy.draft", request, out, func() error {
			o, err := readObjectiveShared(ctx, tx, id)
			if err != nil {
				return err
			}
			if o.State == domain.StateDisabled {
				return failure(409, "objective_disabled")
			}
			p, err := lockPolicy(ctx, tx, rc.Tenant, id)
			if err != nil {
				return err
			}
			if p.draftVersion != expected {
				return failureDetail(409, "version_conflict", fmt.Sprintf("当前草案版本 %d", p.draftVersion))
			}
			if p.locked() != "" {
				return failure(409, "draft_locked")
			}
			tpl, dtpl, err := s.policyTemplate(o)
			if err != nil {
				return err
			}
			st, err := readState(ctx, tx, rc.Tenant, false)
			if err != nil {
				return err
			}
			profile, err := readProfile(ctx, tx, rc.Tenant, st.Profile)
			if err != nil {
				return err
			}
			// 未修改的参数不按当前档案重新校验：基准为当前草案、生效版本或目标参数
			keep := o.Params
			if p.draftRevision != nil {
				r, err := readRevision(ctx, tx, id, *p.draftRevision)
				if err != nil {
					return err
				}
				keep = r.form.Params
			} else if p.active > 0 {
				v, err := readVersion(ctx, tx, id, p.active)
				if err != nil {
					return err
				}
				keep = v.Form.Params
			}
			form, err := normalizeForm(s.Catalog, tpl, dtpl, in, profile, keep)
			if err != nil {
				return err
			}
			if err := s.saveRevision(ctx, tx, rc.Tenant, rc.Principal.ID, o, p, tpl, form); err != nil {
				return err
			}
			st2, err := readPolicyState(ctx, tx, id)
			if err != nil {
				return err
			}
			*out = *st2
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DiscardPolicyDraft 放弃草案（can_draft_policy）。
func (s *Service) DiscardPolicyDraft(ctx context.Context, id string, expected int64, key string) (*PolicyState, error) {
	rc, _, err := s.requireObjective(ctx, id, "can_draft_policy")
	if err != nil {
		return nil, err
	}
	out := &PolicyState{}
	request := struct {
		ID       string `json:"id"`
		Expected int64  `json:"expected_version"`
	}{id, expected}
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return command(ctx, tx, key, "policy.discard", request, out, func() error {
			p, err := lockPolicy(ctx, tx, rc.Tenant, id)
			if err != nil {
				return err
			}
			if p.draftRevision == nil {
				return failure(409, "no_draft")
			}
			if p.draftVersion != expected {
				return failureDetail(409, "version_conflict", fmt.Sprintf("当前草案版本 %d", p.draftVersion))
			}
			if p.locked() != "" {
				return failure(409, "draft_locked")
			}
			revision := *p.draftRevision
			if _, err := tx.Exec(ctx, `UPDATE ctx_policies SET draft_revision = NULL, draft_version = draft_version + 1, updated_at = now() WHERE objective_id = $1`, id); err != nil {
				return err
			}
			if err := recorder.Record(ctx, tx, audit.Entry{Action: "customer_context.policy.draft_discarded", Resource: fmt.Sprintf("%s/r%d", id, revision),
				Result: "succeeded", Basis: "can_draft_policy"}); err != nil {
				return err
			}
			st, err := readPolicyState(ctx, tx, id)
			if err != nil {
				return err
			}
			*out = *st
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// versionCursor 是版本历史的翻页位置：上一页最后一个版本号与签发它的目标的摘要。
type versionCursor struct {
	Version int64  `json:"v"`
	Filter  string `json:"f"`
}

func objectiveFilter(objective string) string {
	sum := sha256.Sum256([]byte(objective))
	return hex.EncodeToString(sum[:6])
}

// VersionPage 是一页版本历史。
type VersionPage struct {
	Versions   []VersionRec
	Truncated  bool
	NextCursor string
}

// ListPolicyVersions 按版本号倒序分页列出版本历史（can_view）。
func (s *Service) ListPolicyVersions(ctx context.Context, id string, limit int, cursor string) (*VersionPage, error) {
	if limit == 0 {
		limit = maxListedVersions
	}
	if limit < 1 || limit > maxListedVersions {
		return nil, failure(400, "invalid_request")
	}
	before := int64(1<<62 - 1)
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var c versionCursor
		if err != nil || json.Unmarshal(raw, &c) != nil || c.Version < 1 || c.Filter != objectiveFilter(id) {
			return nil, failure(400, "invalid_cursor")
		}
		before = c.Version
	}
	if _, _, err := s.requireObjective(ctx, id, "can_view"); err != nil {
		return nil, err
	}
	out := &VersionPage{Versions: []VersionRec{}}
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+versionColumns+` FROM ctx_policy_versions WHERE objective_id = $1 AND version < $2 ORDER BY version DESC LIMIT `+strconv.Itoa(limit+1), id, before)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*VersionRec, error) { return scanVersion(r) })
		if err != nil {
			return err
		}
		if len(list) > limit {
			list, out.Truncated = list[:limit], true
			raw, _ := json.Marshal(versionCursor{Version: list[limit-1].Version, Filter: objectiveFilter(id)})
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		}
		for _, v := range list {
			out.Versions = append(out.Versions, *v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetPolicyVersion 读取一个版本（can_view）。
func (s *Service) GetPolicyVersion(ctx context.Context, id string, version int64) (*VersionRec, error) {
	if _, _, err := s.requireObjective(ctx, id, "can_view"); err != nil {
		return nil, err
	}
	var out *VersionRec
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = readVersion(ctx, tx, id, version)
		return err
	})
	return out, err
}

// side 读取差异的一侧：v<版本号> 或 draft。
func side(ctx context.Context, tx pgx.Tx, objective, ref string) (policySide, error) {
	if ref == "draft" {
		p, err := readPolicyRow(ctx, tx, objective, false)
		if err != nil {
			return policySide{}, err
		}
		if p.draftRevision == nil {
			return policySide{}, failure(404, "no_draft")
		}
		r, err := readRevision(ctx, tx, objective, *p.draftRevision)
		if err != nil {
			return policySide{}, err
		}
		out := policySide{ref: ref, templateVersion: r.templateVersion, form: r.form, rules: r.rules, domains: []string{}}
		sim, err := latestSimulation(ctx, tx, objective, r.revision)
		if err != nil {
			return policySide{}, err
		}
		if sim != nil && sim.Status == "completed" {
			out.domains = sim.Domains
		}
		return out, nil
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(ref, "v"), 10, 64)
	if err != nil || !strings.HasPrefix(ref, "v") || n < 1 {
		return policySide{}, failure(400, "invalid_request")
	}
	v, err := readVersion(ctx, tx, objective, n)
	if err != nil {
		return policySide{}, err
	}
	return policySide{ref: ref, templateVersion: v.TemplateVersion, form: v.Form, rules: v.Rules, domains: v.Domains}, nil
}

// DiffPolicyVersions 给出两个版本（或版本与草案）的规则级差异（can_view）。
func (s *Service) DiffPolicyVersions(ctx context.Context, id, from, to string) (*PolicyDiff, error) {
	if _, _, err := s.requireObjective(ctx, id, "can_view"); err != nil {
		return nil, err
	}
	var out PolicyDiff
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		a, err := side(ctx, tx, id, from)
		if err != nil {
			return err
		}
		b, err := side(ctx, tx, id, to)
		if err != nil {
			return err
		}
		out = diffPolicies(a, b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SimulationInput 是 learning 读取的仿真输入。Document 与 Compare.Document 为合成文档（kernel/policy 的 JSON）。
type SimulationInput struct {
	ObjectiveID     string            `json:"objective_id"`
	OrgUnitID       string            `json:"org_unit_id"`
	RequiresDomains []string          `json:"requires_domains"`
	Revision        int64             `json:"revision"`
	Valid           bool              `json:"valid"`
	Document        json.RawMessage   `json:"document"`
	Form            PolicyForm        `json:"form"`
	Rules           []RuleView        `json:"rules"`
	Mismatches      []ExampleMismatch `json:"example_mismatches"`
	Generation      int64             `json:"generation"`
	Compare         *CompareInput     `json:"compare,omitempty"`
}

// CompareInput 是对比一侧（读取时的生效版本）。
type CompareInput struct {
	Version  int64           `json:"version"`
	Document json.RawMessage `json:"document"`
	Domains  []string        `json:"enabled_domains"`
}

// SimulationInputFor 是 api.PolicyReader：有 can_read_context 的服务主体读取草案修订与读取时的生效版本（只供服务主体）。
func (s *Service) SimulationInputFor(ctx context.Context, id string, revision int64) (*SimulationInput, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Principal.Kind != "service" {
		return nil, failure(403, "forbidden")
	}
	if _, err := s.requireReader(ctx); err != nil {
		return nil, err
	}
	var out *SimulationInput
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		o, err := scanObjective(tx.QueryRow(ctx, `SELECT `+objectiveColumns+` FROM ctx_objectives WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "objective_not_found")
		}
		if err != nil {
			return err
		}
		r, err := readRevision(ctx, tx, id, revision)
		if err != nil {
			return err
		}
		p, err := readPolicyRow(ctx, tx, id, false)
		if err != nil {
			return err
		}
		def, _ := s.Catalog.Objective(o.TemplateID)
		out = &SimulationInput{ObjectiveID: id, OrgUnitID: o.OrgUnitID, RequiresDomains: append([]string{}, def.RequiresDomains...), Revision: revision,
			Valid: r.valid, Document: r.document, Form: r.form, Rules: r.rules, Mismatches: r.mismatches, Generation: p.generation}
		if p.active > 0 {
			var doc []byte
			c := &CompareInput{Version: p.active}
			if err := tx.QueryRow(ctx, `SELECT document, enabled_domains FROM ctx_policy_versions WHERE objective_id = $1 AND version = $2`, id, p.active).Scan(&doc, &c.Domains); err != nil {
				return err
			}
			c.Document = doc
			out.Compare = c
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AttachSimulation 是 learning.simulation.completed.v1 的消费处理：把仿真运行附在草案修订上（按运行标识幂等）。
// 修订不存在（不应发生）时确认事件。
func (s *Service) AttachSimulation(ctx context.Context, tx pgx.Tx, env *outbox.Envelope) error {
	var d struct {
		SimulationID      string    `json:"simulation_id"`
		ObjectiveID       string    `json:"objective_id"`
		Revision          int64     `json:"revision"`
		Status            string    `json:"status"`
		FailureReason     *string   `json:"failure_reason"`
		CompareVersion    int64     `json:"compare_version"`
		CompareGeneration int64     `json:"compare_generation"`
		Domains           []string  `json:"enabled_domains"`
		CompletedAt       time.Time `json:"completed_at"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	rc, _ := reqctx.From(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ctx_policy_revisions WHERE objective_id = $1 AND revision = $2)`, d.ObjectiveID, d.Revision).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if d.Domains == nil {
		d.Domains = []string{}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO ctx_policy_simulations (simulation_id, tenant_id, objective_id, revision, status, failure_reason, compare_version, compare_generation, enabled_domains, completed_at)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (simulation_id) DO NOTHING`, d.SimulationID, rc.Tenant, d.ObjectiveID, d.Revision, d.Status, d.FailureReason,
		d.CompareVersion, d.CompareGeneration, d.Domains, d.CompletedAt)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return recorder.Record(ctx, tx, audit.Entry{Action: "customer_context.policy.simulation_attached", Resource: fmt.Sprintf("%s/r%d/%s", d.ObjectiveID, d.Revision, d.SimulationID),
		Result: "succeeded", Basis: "learning.simulation.completed"})
}
