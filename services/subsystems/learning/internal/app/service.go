// Package app 是 learning 的用例编排：策略仿真的授权、输入读取、运行登记、发件箱与审计。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	commerce "github.com/superailabs/miaoqia/services/subsystems/commerce/api"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api"
)

// SimulationCompleted 是仿真结束（完成或失败）的事件。
const SimulationCompleted = "learning.simulation.completed.v1"

// Service 是 learning 的用例。Access、Context 与 Policies 是 customer-context 的目标授权、上下文与策略仿真输入读取（均经绑定，
// 以 svc:learning 读取）；Dataset 与 Domains 是仿真端口（api.NotWired 为未接入）；Features 是应用领域包的共享特征。
// Authz（identity 的授权决策点）与 Coverage（commerce 的覆盖判断）用于按查看者过滤代表样本，任一缺失时样本全部不返回（samples.go）。
type Service struct {
	DB       *db.DB
	Access   customercontext.ObjectiveAccess
	Scope    customercontext.ObjectiveScope // 目标所在单元（结果汇总的单元授权，C-10）
	Authz    authz.Decider
	Coverage commerce.Coverage
	Context  customercontext.ContextReader
	Policies customercontext.PolicyReader
	Dataset  api.Dataset
	Domains  api.DomainStatus
	Features map[string]policy.Feature
	Hints    map[string]*ReasonHint // 键为“目标编号/反馈结果/原因标签”
	Now      func() time.Time
	Log      *slog.Logger
}

func failure(status int, code string) error { return &api.Error{Status: status, Code: code} }

func (s *Service) businessNow() time.Time {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return now().UTC().Truncate(time.Second)
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// tx 在租户事务中执行；基础设施错误不外泄。
func (s *Service) tx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if s == nil || s.DB == nil {
		return failure(503, "learning_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.DB.Tx(ctx, func(tx pgx.Tx) error { return fn(ctx, tx) })
	var safe *api.Error
	if err == nil || errors.As(err, &safe) {
		return err
	}
	return failure(503, "learning_unavailable")
}

func tenantContext(ctx context.Context) (reqctx.Context, error) {
	rc, ok := reqctx.From(ctx)
	if !ok {
		return rc, reqctx.ErrMissing
	}
	if rc.Validate() != nil || rc.Tenant == "" {
		return rc, failure(403, "invalid_tenant_context")
	}
	return rc, nil
}

// access 经 customer-context（目标的所有者）按其当前口径判断用户主体对目标的关系：可经参与关系获得的权限与本地当前的
// 负责人/参与人集合取交集，移除参与人在 customer-context 提交后立即撤权，不等待 identity 同步；决策者、研究分析者等单元角色
// 不需要是参与人。返回被允许的关系；不能查看与不存在时 visible 为假。判断失败按失败关闭处理（503，不当作无权限）。
func (s *Service) access(ctx context.Context, objective, relation string) (visible, allowed bool, err error) {
	if s.Access == nil {
		return false, false, failure(503, "customer_context_unavailable")
	}
	granted, err := s.Access.Access(ctx, objective, []string{relation})
	if err != nil {
		var safe *customercontext.Error
		switch {
		case errors.As(err, &safe) && safe.Status == 404:
			return false, false, nil
		case errors.As(err, &safe) && safe.Status == 503 && safe.Code != "":
			return false, false, failure(503, safe.Code)
		case errors.As(err, &safe) && safe.Status < 500:
			return false, false, failure(safe.Status, safe.Code)
		}
		return false, false, failure(503, "customer_context_unavailable")
	}
	return true, slices.Contains(granted, relation), nil
}

// Inputs 是报告的固定输入（契约 SimulationInputs）。
type Inputs struct {
	Purpose               string           `json:"purpose"`
	DatasetVersion        string           `json:"dataset_version"`
	DatasetFixture        bool             `json:"dataset_fixture"`
	WindowDays            int              `json:"window_days"`
	WindowStart           time.Time        `json:"window_start"`
	WindowEnd             time.Time        `json:"window_end"`
	ContextVersion        int64            `json:"context_version"`
	OrgUnitID             string           `json:"org_unit_id"`
	Revision              int64            `json:"revision"`
	TemplateID            string           `json:"template_id"`
	TemplateVersion       string           `json:"template_version"`
	Generation            int64            `json:"generation"`
	EnabledDomains        []string         `json:"enabled_domains"`
	ExcludedDomains       []ExcludedDomain `json:"excluded_domains"`
	CompareVersion        int64            `json:"compare_version"`
	CompareEnabledDomains []string         `json:"compare_enabled_domains,omitempty"`
}

// ExcludedDomain 是目标需要但未启用的信息领域。
type ExcludedDomain struct {
	Domain string `json:"domain"`
	Status string `json:"status"`
}

// Run 是一次仿真运行（契约 SimulationRun）。
type Run struct {
	ID            string     `json:"id"`
	ObjectiveID   string     `json:"objective_id"`
	Revision      int64      `json:"revision"`
	Status        string     `json:"status"`
	FailureReason string     `json:"failure_reason,omitempty"`
	Inputs        *Inputs    `json:"inputs,omitempty"`
	Report        *Report    `json:"report,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// runFailure 是仿真运行的失败原因（运行照常登记为 failed）。
type runFailure struct{ reason string }

func (f *runFailure) Error() string { return f.reason }

// prepared 是准备好的仿真：固定输入、输入键与计算。
type prepared struct {
	objective         string
	revision          int64
	inputs            *Inputs
	key               string
	comp              *computation
	compareGeneration int64
}

func document(raw cc.PolicyDocument) (*policy.Policy, error) {
	b, err := json.Marshal(&raw)
	if err != nil {
		return nil, err
	}
	var p policy.Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// prepare 读取仿真输入（以 svc:learning）并确定固定输入。返回 *runFailure 时运行登记为失败；其他错误原样返回。
func (s *Service) prepare(ctx context.Context, objective string, revision int64) (*prepared, error) {
	sctx := reqctx.Service(ctx, api.Name)
	if s.Policies == nil {
		return nil, &runFailure{"policy_unavailable"}
	}
	in, err := s.Policies.SimulationInput(sctx, objective, revision)
	if err != nil {
		var safe *customercontext.Error
		if errors.As(err, &safe) && safe.Status == 404 {
			return nil, failure(404, "simulation_target_not_found")
		}
		return nil, &runFailure{"policy_unavailable"}
	}
	if !in.Valid {
		return nil, failure(409, "policy_invalid")
	}
	draftDoc, err := document(in.Document)
	if err != nil {
		return nil, &runFailure{"policy_unavailable"}
	}
	draft, issues := policy.Compile(draftDoc, s.Features, policy.DefaultLimits())
	if len(issues) > 0 {
		return nil, failure(409, "policy_invalid")
	}
	comp := &computation{draft: side{compiled: draft}, features: s.Features, lists: map[string]any{"customers": []any{}, "blocked": []any{}, "competitors": []any{}}}
	p := &prepared{objective: objective, revision: revision, comp: comp, compareGeneration: in.Generation}
	inputs := &Inputs{Purpose: "counterfactual_candidate", OrgUnitID: in.OrgUnitID, Revision: revision, TemplateID: draftDoc.ID,
		TemplateVersion: draftDoc.Version, Generation: in.Generation, ExcludedDomains: []ExcludedDomain{}}
	if c, ok := in.Compare.Get(); ok {
		doc, err := document(c.Document)
		if err != nil {
			return nil, &runFailure{"policy_unavailable"}
		}
		compiled, issues := policy.Compile(doc, s.Features, policy.DefaultLimits())
		if len(issues) > 0 {
			return nil, &runFailure{"policy_unavailable"}
		}
		domains := domainCodes(c.EnabledDomains)
		comp.compare = &side{compiled: compiled, domains: domains}
		inputs.CompareVersion, inputs.CompareEnabledDomains = c.Version, domains
	}
	for _, m := range in.ExampleMismatches {
		comp.mismatches = append(comp.mismatches, m.Name)
	}
	for _, r := range in.Rules {
		if r.Clause == "require" && !r.Enabled {
			comp.disabledReq = append(comp.disabledReq, string(r.ID))
		}
	}
	// 启用领域集合：目标需要的领域中服务状态为 live 的；其余为未启用（not_live 或 unknown）
	if s.Domains == nil {
		return nil, &runFailure{"domain_status_unavailable"}
	}
	required := domainCodes(in.RequiresDomains)
	status, err := s.Domains.Status(sctx, required)
	if err != nil {
		return nil, &runFailure{"domain_status_unavailable"}
	}
	inputs.EnabledDomains = []string{}
	for _, d := range required {
		switch status[d] {
		case api.DomainLive:
			inputs.EnabledDomains = append(inputs.EnabledDomains, d)
		case api.DomainNotLive:
			inputs.ExcludedDomains = append(inputs.ExcludedDomains, ExcludedDomain{Domain: d, Status: "not_live"})
		default:
			inputs.ExcludedDomains = append(inputs.ExcludedDomains, ExcludedDomain{Domain: d, Status: "unknown"})
		}
	}
	comp.draft.domains = inputs.EnabledDomains
	// 客户上下文：读取时的当前版本，两侧共用同一快照
	if s.Context == nil {
		return nil, &runFailure{"context_unavailable"}
	}
	version, err := s.Context.ContextVersion(sctx)
	if err != nil {
		return nil, &runFailure{"context_unavailable"}
	}
	snapshot, err := s.Context.ContextSnapshot(sctx, in.OrgUnitID, version.ContextVersion)
	if err != nil {
		return nil, &runFailure{"context_unavailable"}
	}
	inputs.ContextVersion = snapshot.ContextVersion
	comp.profile = profileOf(snapshot)
	// 窗口：截至业务日期零时（UTC）的近90天，可得历史不足时按实际可得截断
	if s.Dataset == nil {
		return nil, &runFailure{"dataset_unavailable"}
	}
	end := s.businessNow().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -windowDays)
	slice, err := s.Dataset.Load(sctx, start, end)
	if err != nil {
		return nil, &runFailure{"dataset_unavailable"}
	}
	if slice.CoverageStart.After(start) {
		start = slice.CoverageStart.UTC()
	}
	if !start.Before(end) {
		start = end.Add(-24 * time.Hour)
	}
	comp.slice, comp.from, comp.to = slice, start, end
	comp.days = int(math.Ceil(end.Sub(start).Hours() / 24))
	inputs.DatasetVersion, inputs.DatasetFixture = slice.Version, slice.Fixture
	inputs.WindowDays, inputs.WindowStart, inputs.WindowEnd = comp.days, start, end
	p.inputs = inputs
	key, err := json.Marshal(struct {
		Objective string  `json:"o"`
		Inputs    *Inputs `json:"i"`
	}{objective, inputs})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	p.key = hex.EncodeToString(sum[:])
	return p, nil
}

func domainCodes(in []cc.DomainCode) []string {
	out := []string{}
	for _, d := range in {
		if !slices.Contains(out, string(d)) {
			out = append(out, string(d))
		}
	}
	slices.Sort(out)
	return out
}

// profileOf 把快照的策略视图转换为求值输入的 profile：未知的字段（如没有我方主体时的 org_ids）不出现，读取为未知。
func profileOf(s *customercontext.Snapshot) map[string]any {
	list := func(v []string) []any {
		out := []any{}
		for _, x := range v {
			out = append(out, x)
		}
		return out
	}
	v := s.PolicyView
	out := map[string]any{"offering_categories": list(v.OfferingCategories), "service_regions": list(v.ServiceRegions), "brands": list(v.Brands)}
	if v.OrgIds != nil {
		out["org_ids"] = list(v.OrgIds)
	}
	return out
}

const runColumns = `id, objective_id, revision, status, failure_reason, inputs, report, created_at, completed_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	var reason *string
	var inputs, report []byte
	var completed time.Time
	if err := row.Scan(&r.ID, &r.ObjectiveID, &r.Revision, &r.Status, &reason, &inputs, &report, &r.CreatedAt, &completed); err != nil {
		return nil, err
	}
	r.CreatedAt, completed = r.CreatedAt.UTC(), completed.UTC()
	r.CompletedAt = &completed
	if reason != nil {
		r.FailureReason = *reason
	}
	if inputs != nil {
		r.Inputs = &Inputs{}
		if err := json.Unmarshal(inputs, r.Inputs); err != nil {
			return nil, err
		}
	}
	if report != nil {
		r.Report = &Report{}
		if err := json.Unmarshal(report, r.Report); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

var (
	writer   = outbox.Writer{Subsystem: api.Name, Namespace: "learning", Zone: reqctx.ZoneTenant}
	recorder = audit.Recorder{Subsystem: api.Name}
)

// record 登记运行并发布 learning.simulation.completed（同一事务）。完成的运行按输入键加事务级咨询锁：并发的相同输入只登记一次，
// 后到者返回已登记的运行。
func (s *Service) record(ctx context.Context, rc reqctx.Context, objective string, revision int64, p *prepared, report *Report, reason string) (*Run, error) {
	var out *Run
	err := s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if p != nil {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "lrn-simulation/"+rc.Tenant+"/"+p.key); err != nil {
				return err
			}
			existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM lrn_simulation_runs WHERE tenant_id = $1 AND input_key = $2 AND status = 'completed'`, rc.Tenant, p.key))
			if err == nil {
				out = existing
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		id, completed := ids.New("sim"), s.businessNow()
		status := "completed"
		var key, failure *string
		var inputs, rep []byte
		if reason != "" {
			status, failure = "failed", &reason
		} else {
			key = &p.key
			var err error
			if inputs, err = json.Marshal(p.inputs); err != nil {
				return err
			}
			if rep, err = json.Marshal(report); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lrn_simulation_runs (id, tenant_id, objective_id, revision, status, failure_reason, input_key, inputs, report, requested_by, completed_at)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, id, rc.Tenant, objective, revision, status, failure, key, inputs, rep, rc.Principal.ID, completed); err != nil {
			return err
		}
		data := map[string]any{"simulation_id": id, "objective_id": objective, "revision": revision, "status": status,
			"compare_version": int64(0), "compare_generation": int64(0), "enabled_domains": []string{}, "completed_at": completed.Format(time.RFC3339Nano)}
		if p != nil {
			data["compare_version"], data["compare_generation"], data["enabled_domains"] = p.inputs.CompareVersion, p.compareGeneration, p.inputs.EnabledDomains
		}
		if reason != "" {
			data["failure_reason"] = reason
		}
		if _, err := writer.Append(ctx, tx, outbox.Event{Type: SimulationCompleted, Subject: id, PartitionKey: objective, Data: data}); err != nil {
			return err
		}
		if err := recorder.Record(ctx, tx, audit.Entry{Action: "learning.simulation." + status, Resource: fmt.Sprintf("%s/r%d/%s", objective, revision, id),
			Result: "succeeded", Basis: "can_draft_policy"}); err != nil {
			return err
		}
		var err error
		out, err = scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM lrn_simulation_runs WHERE id = $1`, id))
		return err
	})
	return out, err
}

// simulate 运行一次仿真：相同输入的完成运行直接复用；输入读取失败时登记失败的运行（仍附到修订上，不能用于申请）。
func (s *Service) simulate(ctx context.Context, rc reqctx.Context, objective string, revision int64) (*Run, error) {
	p, err := s.prepare(ctx, objective, revision)
	var failed *runFailure
	if errors.As(err, &failed) {
		s.logger().WarnContext(ctx, "策略仿真失败", "subsystem", api.Name, "objective", objective, "revision", revision, "reason", failed.reason)
		return s.record(ctx, rc, objective, revision, nil, nil, failed.reason)
	}
	if err != nil {
		return nil, err
	}
	// 先查已有的相同输入运行，避免重复计算
	var existing *Run
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM lrn_simulation_runs WHERE tenant_id = $1 AND input_key = $2 AND status = 'completed'`, rc.Tenant, p.key))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		existing = r
		return err
	})
	if err != nil || existing != nil {
		return existing, err
	}
	return s.record(ctx, rc, objective, revision, p, p.comp.run(), "")
}

// SimulatePolicy 对草案修订运行仿真（目标的 can_draft_policy，按 customer-context 的当前口径；契约 simulatePolicy）。无权查看目标时与修订不存在同样 404。
func (s *Service) SimulatePolicy(ctx context.Context, objective string, revision int64) (*Run, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Principal.Kind != "user" {
		return nil, failure(403, "forbidden")
	}
	visible, ok, err := s.access(ctx, objective, "can_draft_policy")
	if err != nil {
		return nil, err
	}
	if !visible {
		return nil, failure(404, "simulation_target_not_found")
	}
	if !ok {
		return nil, failure(403, "forbidden")
	}
	run, err := s.simulate(ctx, rc, objective, revision)
	return s.present(ctx, run), err
}

// GetSimulation 读取运行（目标的 can_view，按 customer-context 的当前口径）；无权限、其他租户的与不存在的运行返回同一 404。
func (s *Service) GetSimulation(ctx context.Context, id string) (*Run, error) {
	rc, err := tenantContext(ctx)
	if err != nil {
		return nil, err
	}
	if rc.Principal.Kind != "user" {
		return nil, failure(404, "simulation_not_found")
	}
	var out *Run
	err = s.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM lrn_simulation_runs WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(404, "simulation_not_found")
		}
		out = r
		return err
	})
	if err != nil {
		return nil, err
	}
	visible, _, err := s.access(ctx, out.ObjectiveID, "can_view")
	if err != nil {
		return nil, err
	}
	if !visible {
		return nil, failure(404, "simulation_not_found")
	}
	return s.present(ctx, out), nil
}

// PolicyDrafted 是 customer_context.policy.drafted.v1 的消费处理：通过校验的草案修订保存后自动仿真（以 svc:learning）。
// 输入读取失败登记为失败的运行并确认事件；本库不可用时返回错误，事件按退避重试。
func (s *Service) PolicyDrafted(ctx context.Context, _ pgx.Tx, env *outbox.Envelope) error {
	var d struct {
		ObjectiveID string `json:"objective_id"`
		Revision    int64  `json:"revision"`
		Valid       bool   `json:"valid"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	if !d.Valid {
		return nil
	}
	rc, err := tenantContext(ctx)
	if err != nil {
		return err
	}
	_, err = s.simulate(ctx, rc, d.ObjectiveID, d.Revision)
	var safe *api.Error
	if errors.As(err, &safe) && safe.Status < 500 {
		return nil // 修订不存在或未通过校验：没有可仿真的内容
	}
	return err
}
