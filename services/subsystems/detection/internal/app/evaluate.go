package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	cc "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/ids"
	"github.com/superailabs/miaoqia/services/kernel/outbox"
	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	customercontext "github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// 生效边界上的匹配（detection.md 第8节；PRD 第7段第5节）：每个领域事件对每个候选租户是一个批次。批次开始时读主库的生效策略与
// 策略代次（GetActivePolicies，全部分页）与上下文版本，任一读取失败时返回错误，事件按消费框架退避重试，不用可能过期的缓存求值。
// 激活或回滚返回后开始的批次因此一律使用新版本；customer_context.policy.activated / rolled_back 事件不参与正确性，本切片不消费。
// 编译结果与上下文快照按不可变的键缓存（目标、版本与文档摘要；租户、组织单元与上下文版本），缓存只省去重复编译与读取，
// 不决定使用哪个版本。

// activePolicyPage 是读取生效策略的每页条数（契约上限）。
const activePolicyPage = 200

var tenantPattern = regexp.MustCompile(`^tnt_[0-9A-HJKMNP-TV-Z]{26}$`)

// lists 是求值输入的租户清单。客户、禁合作与竞争对手清单随清单导入实现（customer-context），此前为空：
// 引用清单的规则按“不在清单中”求值（与 learning 仿真一致）。
var lists = map[string]any{"customers": []any{}, "blocked": []any{}, "competitors": []any{}}

// FactUpserted 是 domain.fact.upserted.v1（公共域）的消费处理。公共事件没有租户：每个候选租户在各自的租户事务中处理
// （与消费框架的收件事务分开），匹配记录以（目标、求值单位、事实版本）唯一，重试只补做尚未完成的租户与目标。
func (s *Service) FactUpserted(ctx context.Context, _ pgx.Tx, env *outbox.Envelope) error {
	var d struct {
		FactID        string   `json:"fact_id"`
		FactVersion   int64    `json:"fact_version"`
		ObjectType    string   `json:"object_type"`
		InfoDomain    string   `json:"info_domain"`
		ChangedFields []string `json:"changed_fields"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return err
	}
	ref := api.FactRef{ID: d.FactID, Version: d.FactVersion, ObjectType: d.ObjectType, InfoDomain: d.InfoDomain, ChangedFields: d.ChangedFields,
		EventID: env.ID, EventTime: env.Time.UTC()}
	if s.Candidates == nil || s.Facts == nil {
		return api.ErrNotWired
	}
	tenants, err := s.Candidates.Tenants(ctx, s.Cell, ref)
	if err != nil {
		return fmt.Errorf("订阅反查：%w", err)
	}
	if len(tenants) == 0 {
		return nil
	}
	units, err := s.Facts.Units(ctx, ref)
	if err != nil {
		return fmt.Errorf("视图组装：%w", err)
	}
	if len(units) == 0 {
		return nil
	}
	slices.Sort(tenants)
	for _, tenant := range slices.Compact(tenants) {
		if !tenantPattern.MatchString(tenant) {
			s.logger().WarnContext(ctx, "订阅反查返回非法租户，跳过", "tenant", tenant)
			continue
		}
		tctx := reqctx.Service(reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: s.Cell, Purpose: "monitoring"}), api.Name)
		if err := s.evaluateTenant(tctx, ref, units); err != nil {
			return fmt.Errorf("租户 %s：%w", tenant, err)
		}
	}
	return nil
}

// active 是批次开始时读取的一个生效策略。
type active struct {
	p        cc.ActivePolicy
	compiled *policy.Compiled
	types    []string
}

// readPolicies 读取全部生效策略（按目标分页，直到没有下一页）。
func (s *Service) readPolicies(ctx context.Context) ([]cc.ActivePolicy, error) {
	if s.Policies == nil {
		return nil, errors.New("生效策略读取未组装")
	}
	var out []cc.ActivePolicy
	cursor := ""
	for range 10000 {
		page, err := s.Policies.ActivePolicies(ctx, activePolicyPage, cursor)
		if err != nil {
			return nil, fmt.Errorf("读取生效策略：%w", err)
		}
		out = append(out, page.Policies...)
		next, ok := page.NextCursor.Get()
		if !ok || next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, errors.New("生效策略分页没有结束")
}

func documentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// compile 返回生效版本的编译结果（按目标、版本与文档摘要缓存）。编译失败是生效版本的数据错误：返回错误，事件重试后转入死信并告警。
func (s *Service) compile(p cc.ActivePolicy) (*policy.Compiled, error) {
	raw, err := json.Marshal(&p.Document)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s/%d/%s", p.ObjectiveID, p.Version, documentHash(raw))
	if c, ok := s.compiled.Load(key); ok {
		return c.(*policy.Compiled), nil
	}
	var doc policy.Policy
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	compiled, issues := policy.Compile(&doc, s.Packs.Features, policy.DefaultLimits())
	if len(issues) > 0 {
		return nil, fmt.Errorf("目标 %s 的生效版本 %d 编译失败：%s", p.ObjectiveID, p.Version, issues[0].String())
	}
	s.compiled.Store(key, compiled)
	return compiled, nil
}

// snapshot 返回（组织单元、上下文版本）的上下文快照（不可变，按租户、组织单元与版本缓存）。
func (s *Service) snapshot(ctx context.Context, tenant, unit string, version int64) (*customercontext.Snapshot, error) {
	key := fmt.Sprintf("%s/%s/%d", tenant, unit, version)
	if v, ok := s.snapshots.Load(key); ok {
		return v.(*customercontext.Snapshot), nil
	}
	snap, err := s.Context.ContextSnapshot(ctx, unit, version)
	if err != nil {
		return nil, fmt.Errorf("读取上下文快照：%w", err)
	}
	s.snapshots.Store(key, snap)
	return snap, nil
}

func (s *Service) evaluateTenant(ctx context.Context, ref api.FactRef, units []api.Unit) error {
	rc, _ := reqctx.From(ctx)
	policies, err := s.readPolicies(ctx)
	if err != nil {
		return err
	}
	var batch []active
	for _, p := range policies {
		if p.ObjectiveState != cc.ActivePolicyObjectiveStateActive {
			continue // 未激活或已停用的目标不产生新信号；已有信号保留
		}
		compiled, err := s.compile(p)
		if err != nil {
			return err
		}
		types := compiled.Policy.Scope.FactTypes
		if len(types) == 0 {
			types = compiled.Policy.FactTypes
		}
		batch = append(batch, active{p: p, compiled: compiled, types: types})
	}
	relevant := false
	for _, a := range batch {
		for _, u := range units {
			relevant = relevant || slices.Contains(a.types, u.FactType)
		}
	}
	if !relevant {
		return nil
	}
	if s.Context == nil {
		return errors.New("上下文读取未组装")
	}
	version, err := s.Context.ContextVersion(ctx)
	if err != nil {
		return fmt.Errorf("读取上下文版本：%w", err)
	}
	for _, a := range batch {
		var snap *customercontext.Snapshot
		for _, u := range units {
			if !slices.Contains(a.types, u.FactType) || !slices.Contains(domainCodes(a.p.EnabledDomains), u.Domain) {
				continue // 事实类型不在策略范围内，或其信息领域不在该版本的启用领域集合中
			}
			if snap == nil {
				if snap, err = s.snapshot(ctx, rc.Tenant, a.p.OrgUnitID, version.ContextVersion); err != nil {
					return err
				}
			}
			if err := s.match(ctx, ref, a, snap, u); err != nil {
				return err
			}
		}
	}
	return nil
}

func domainCodes(in []cc.DomainCode) []string {
	out := []string{}
	for _, d := range in {
		out = append(out, string(d))
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

func clone(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func setPath(m map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

// view 是一次求值组装的视图（快照内容）。
type view struct {
	FactType string                  `json:"fact_type"`
	Subject  api.Subject             `json:"subject"`
	Event    string                  `json:"event"`
	Fact     map[string]any          `json:"fact"`
	Evidence map[string]api.Evidence `json:"evidence,omitempty"`
	Items    []api.ItemRef           `json:"items"`
	Withheld []string                `json:"withheld,omitempty"`
}

// assemble 组装求值视图：启用领域集合外的信息领域提供的关联视图项不出现（读取为未知，原因 DOMAIN_NOT_ENABLED）。
func assemble(u api.Unit, enabled []string) *view {
	fact, _ := clone(u.Fields).(map[string]any)
	if fact == nil {
		fact = map[string]any{}
	}
	v := &view{FactType: u.FactType, Subject: u.Subject, Event: u.Event, Evidence: u.Evidence, Items: []api.ItemRef{}}
	names := make([]string, 0, len(u.Items))
	for name := range u.Items {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if d, ok := u.ItemDomains[name]; ok && !slices.Contains(enabled, d) {
			v.Withheld = append(v.Withheld, name)
			continue
		}
		setPath(fact, name, clone(u.Items[name]))
	}
	for _, r := range u.ItemRefs {
		if r.Domain == "" || slices.Contains(enabled, r.Domain) {
			v.Items = append(v.Items, r)
		}
	}
	v.Fact = fact
	return v
}

func (v *view) hash() (string, []byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), raw, nil
}

// 未知原因（契约 RuleTrace.unknown_reason），按优先级排列：一条规则的未知只给一个原因。
var reasonOrder = []string{"DOMAIN_NOT_ENABLED", "CATEGORY_UNCERTAIN", "BELOW_EVIDENCE", "SELF_NOT_LINKED", "FIELD_MISSING"}

// profileFields 是策略视图中来自客户档案的字段：缺失时（如档案没有我方主体时的 org_ids）规则未知的原因为 SELF_NOT_LINKED。
var profileFields = []string{"org_ids"}

// classify 给出规则未知的原因（kernel/policy 的轨迹以视图路径给出证据不达标的字段，以键名给出缺失的字段，以特征名给出未知的特征）：
// 所需关联视图项的领域未启用 → DOMAIN_NOT_ENABLED；品类字段证据不达标 → CATEGORY_UNCERTAIN；其他字段证据不达标 → BELOW_EVIDENCE；
// 我方主体（档案 org_ids）缺失 → SELF_NOT_LINKED；其余 → FIELD_MISSING。HISTORY_INSUFFICIENT 需要关联视图项的覆盖期，本切片不产生。
func classify(features map[string]policy.Feature, v *view, profile map[string]any, fields []string) string {
	best := len(reasonOrder) - 1
	rank := func(r string) {
		if i := slices.Index(reasonOrder, r); i < best {
			best = i
		}
	}
	withheld := func(item string) bool { return slices.Contains(v.Withheld, item) }
	itemOf := func(path string) (string, bool) {
		for _, item := range v.Withheld {
			if path == item || strings.HasPrefix(path, item+".") {
				return item, true
			}
		}
		return "", false
	}
	for _, f := range fields {
		name := strings.TrimPrefix(f, "features.")
		if feature, ok := features[name]; ok {
			for _, item := range feature.View {
				if withheld(item) {
					rank("DOMAIN_NOT_ENABLED")
				}
			}
			continue
		}
		if _, ok := v.Evidence[f]; ok {
			if f == "fact.categories" {
				rank("CATEGORY_UNCERTAIN")
			} else {
				rank("BELOW_EVIDENCE")
			}
			continue
		}
		if _, ok := itemOf(strings.TrimPrefix(f, "fact.")); ok {
			rank("DOMAIN_NOT_ENABLED")
			continue
		}
		key := f[strings.LastIndex(f, ".")+1:]
		for _, item := range v.Withheld {
			if item == key || strings.HasSuffix(item, "."+key) {
				rank("DOMAIN_NOT_ENABLED")
			}
		}
		if _, inProfile := profile[key]; !inProfile && slices.Contains(profileFields, key) && (strings.HasPrefix(f, "profile.") || !strings.Contains(f, ".")) {
			rank("SELF_NOT_LINKED")
		}
	}
	return reasonOrder[best]
}

// Trace 是信号上的一条规则轨迹（契约 RuleTrace）。
type Trace struct {
	ID            string   `json:"id"`
	Clause        string   `json:"clause"`
	Result        string   `json:"result"`
	Say           string   `json:"say,omitempty"`
	Weight        int      `json:"weight,omitempty"`
	UnknownReason string   `json:"unknown_reason,omitempty"`
	UnknownFields []string `json:"unknown_fields,omitempty"`
}

func traceOf(c *policy.Compiled, features map[string]policy.Feature, v *view, profile map[string]any, res *policy.Result) []Trace {
	weights := map[string]int{}
	for _, r := range append(slices.Clone(c.Policy.Boost), c.Policy.Penalize...) {
		weights[r.ID] = r.Weight
	}
	out := []Trace{}
	for _, t := range res.Trace {
		tr := Trace{ID: t.ID, Clause: string(t.Clause), Result: t.Result.String(), Say: t.Say, Weight: weights[t.ID]}
		if t.Result == policy.Unknown {
			tr.UnknownReason = classify(features, v, profile, t.Unknown)
			tr.UnknownFields = t.Unknown
		}
		out = append(out, tr)
	}
	return out
}

// conclusion 是一句话的处置结论与原因（PRD 第3段 F3）。
func conclusion(c *policy.Compiled, res *policy.Result, trace []Trace) string {
	says := func(clause, result string) []string {
		var out []string
		for _, t := range trace {
			if t.Clause == clause && t.Result == result {
				out = append(out, "“"+t.Say+"”")
			}
		}
		return out
	}
	th := c.Policy.Thresholds
	terms := fmt.Sprintf("基础 %d", th.BaseScore)
	for _, t := range trace {
		if t.Result == "true" && t.Clause == "boost" {
			terms += fmt.Sprintf(" + %s %d", t.Say, t.Weight)
		}
		if t.Result == "true" && t.Clause == "penalize" {
			terms += fmt.Sprintf(" − %s %d", t.Say, t.Weight)
		}
	}
	switch res.Disposition {
	case policy.Excluded:
		return fmt.Sprintf("排除：%s（%s）", strings.Join(says("exclude", "true"), "、"), res.Reason)
	case policy.WatchSignal:
		return "观察：" + strings.Join(says("watch", "true"), "、")
	case policy.ReviewCandidate:
		if res.NeedsReview {
			return fmt.Sprintf("复核候选：%s未能确定，需人工复核（得分 %d：%s）", strings.Join(says("require", "unknown"), "、"), res.Score, terms)
		}
		return fmt.Sprintf("复核候选：得分 %d（%s），未达到跟进阈值 %d", res.Score, terms, th.PursueAt)
	default:
		return fmt.Sprintf("跟进候选：硬条件全部满足，得分 %d（%s），达到跟进阈值 %d", res.Score, terms, th.PursueAt)
	}
}

// rowReasons 是收件箱行的至多两条命中理由：排除为生效的排除规则；其余依次取为真的加分（权重大者在前）、观察与硬条件。
func rowReasons(trace []Trace, d policy.Disposition) []string {
	pick := func(clause string) []Trace {
		var out []Trace
		for _, t := range trace {
			if t.Clause == clause && t.Result == "true" && t.Say != "" {
				out = append(out, t)
			}
		}
		return out
	}
	var ordered []Trace
	if d == policy.Excluded {
		ordered = pick("exclude")
	} else {
		boosts := pick("boost")
		slices.SortStableFunc(boosts, func(a, b Trace) int { return cmp.Compare(b.Weight, a.Weight) })
		ordered = append(append(boosts, pick("watch")...), pick("require")...)
	}
	out := []string{}
	for _, t := range ordered {
		if len(out) == 2 {
			break
		}
		out = append(out, t.Say)
	}
	return out
}

// match 对一个目标的生效版本求值一个求值单位并写入匹配记录与信号（同一事务）。同一（目标、求值单位、事实版本）已有匹配记录时
// 不再求值（重复投递、重试或激活与回滚之后的重放都不重算）；求值单位已有其他版本的匹配记录时，只有视图声明的触发事件才重新
// 求值——A02 结果视图没有触发事件，记录 not_triggered（重新求值与 SignalUpdate 随视图触发实现）。
func (s *Service) match(ctx context.Context, ref api.FactRef, a active, snap *customercontext.Snapshot, u api.Unit) error {
	rc, _ := reqctx.From(ctx)
	p := a.p
	return s.DB.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "det-match/"+rc.Tenant+"/"+p.ObjectiveID+"/"+u.Key); err != nil {
			return err
		}
		var exact, other int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE subject_version = $3), count(*) FILTER (WHERE subject_version <> $3)
 FROM det_matches WHERE objective_id = $1 AND unit_key = $2`, p.ObjectiveID, u.Key, u.Subject.Version).Scan(&exact, &other); err != nil {
			return err
		}
		if exact > 0 {
			return nil
		}
		record := func(outcome string, signal *string) error {
			_, err := tx.Exec(ctx, `INSERT INTO det_matches (tenant_id, objective_id, unit_key, subject_version, policy_id, policy_version, generation,
 context_version, outcome, signal_id, trigger_event_id, evaluated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
				rc.Tenant, p.ObjectiveID, u.Key, u.Subject.Version, p.PolicyID, p.Version, p.Generation, snap.ContextVersion, outcome, signal, ref.EventID, ref.EventTime)
			return err
		}
		if other > 0 {
			return record("not_triggered", nil)
		}
		enabled := domainCodes(p.EnabledDomains)
		v := assemble(u, enabled)
		profile := profileOf(snap)
		evidence := map[string]policy.Evidence{}
		for path, e := range u.Evidence {
			evidence[path] = policy.Evidence{Confidence: e.Confidence, Method: e.Method, Suspect: e.Suspect}
		}
		res, err := a.compiled.Evaluate(policy.Input{FactType: u.FactType, Fact: v.Fact, Event: map[string]any{"type": u.Event}, Profile: profile,
			Lists: lists, Vocab: map[string]any{}, Now: ref.EventTime, Evidence: evidence})
		if errors.Is(err, policy.ErrCostExceeded) {
			s.logger().WarnContext(ctx, "求值成本超过上限，不产生信号", "objective", p.ObjectiveID, "unit", u.Key)
			return record("cost_exceeded", nil)
		}
		if err != nil {
			return err
		}
		for _, alert := range res.Alerts {
			s.logger().WarnContext(ctx, "求值告警", "objective", p.ObjectiveID, "unit", u.Key, "alert", alert)
		}
		if res.Disposition == policy.NoSignal {
			return record("no_signal", nil)
		}
		hash, content, err := v.hash()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO det_view_snapshots (tenant_id, hash, content) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, rc.Tenant, hash, content); err != nil {
			return err
		}
		trace := traceOf(a.compiled, s.Packs.Features, v, profile, res)
		unknown := 0
		for _, t := range trace {
			if t.Result == "unknown" {
				unknown++
			}
		}
		traceJSON, _ := json.Marshal(trace)
		features, _ := json.Marshal(res.Features)
		id, created := ids.New("sig"), s.now()
		if err := record(string(res.Disposition), &id); err != nil {
			return err
		}
		var reason *string
		if res.Reason != "" {
			reason = &res.Reason
		}
		d := u.Display
		title := d.Title
		if title == "" {
			title = u.Subject.ID
		}
		var part, lot *string
		if u.Subject.PartID != "" {
			part = &u.Subject.PartID
		}
		if d.LotKey != "" {
			lot = &d.LotKey
		}
		null := func(s string) *string {
			if s == "" {
				return nil
			}
			return &s
		}
		categories := d.Categories
		if categories == nil {
			categories = []string{}
		}
		// 候选负责人（C-14）：按同一次读取的分派规则与负责人、参与人计算；分派规则不可用时两列为空
		var candidate *string
		var assignmentVersion *int64
		if owner, version, ok := candidateOwner(p, d.Location, categories); ok {
			assignmentVersion = &version
			if owner != "" {
				candidate = &owner
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO det_signals (id, tenant_id, objective_id, objective_code, org_unit_id, policy_id, policy_version, generation,
 template_id, template_version, enabled_domains, unit_key, fact_type, subject_id, subject_version, part_id, title, winner_org_id, winner_name,
 amount_value, amount_currency, amount_cny, location, categories, lot_key, disposition, score, needs_review, reason_code, conclusion, reasons,
 unknown_count, trace, features, view_snapshot_hash, context_version, trigger_event_id, evaluated_at, created_at,
 procurement_project_id, buyer_org_id, buyer_name, candidate_owner, assignment_version)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,
 $40,$41,$42,$43,$44)`,
			id, rc.Tenant, p.ObjectiveID, p.ObjectiveCode, p.OrgUnitID, p.PolicyID, p.Version, p.Generation, p.TemplateID, p.TemplateVersion, enabled,
			u.Key, u.FactType, u.Subject.ID, u.Subject.Version, part, title, null(d.WinnerOrgID), null(d.WinnerName), null(d.AmountValue),
			null(d.AmountCurrency), d.AmountCNY, null(d.Location), categories, lot, string(res.Disposition), res.Score, res.NeedsReview, reason,
			conclusion(a.compiled, res, trace), rowReasons(trace, res.Disposition), unknown, traceJSON, features, hash, snap.ContextVersion,
			ref.EventID, ref.EventTime, created, idOrNil(d.ProcurementProjectID, "pp"), idOrNil(d.BuyerOrgID, "org"), null(d.BuyerName),
			candidate, assignmentVersion); err != nil {
			return err
		}
		subject := map[string]any{"type": "domain_fact", "id": u.Subject.ID, "version": u.Subject.Version}
		if u.Subject.PartID != "" {
			subject["part_id"] = u.Subject.PartID
		}
		data := map[string]any{"signal_id": id, "objective_id": p.ObjectiveID, "objective_code": p.ObjectiveCode, "policy_id": p.PolicyID,
			"policy_version": p.Version, "policy_generation": p.Generation, "template_id": p.TemplateID, "template_version": p.TemplateVersion,
			"subject": subject, "fact_type": u.FactType, "disposition": string(res.Disposition), "score": res.Score, "needs_review": res.NeedsReview,
			"context_snapshot_version": snap.ContextVersion, "evaluated_at": ref.EventTime.Format(time.RFC3339Nano),
			"created_at": created.Format(time.RFC3339Nano), "backfill": false}
		if reason != nil {
			data["reason_code"] = *reason
		}
		_, err = writer.Append(ctx, tx, outbox.Event{Type: SignalDetected, Subject: id, PartitionKey: p.ObjectiveID, Data: data})
		return err
	})
}

var typedID = regexp.MustCompile(`^[a-z]{2,3}_[0-9A-HJKMNP-TV-Z]{26}$`)

// idOrNil 返回符合登记前缀格式的标识；视图中缺失或格式不符的标识不记录（不让展示字段的问题阻断信号写入）。
func idOrNil(id, prefix string) *string {
	if !typedID.MatchString(id) || !strings.HasPrefix(id, prefix+"_") {
		return nil
	}
	return &id
}
