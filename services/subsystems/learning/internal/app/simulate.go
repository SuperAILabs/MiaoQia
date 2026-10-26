package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api"
)

// 候选变更仿真的计算（learning.md 8.1）：两侧（草案、对比的生效版本）共用同一数据集切片、窗口、客户上下文快照与逐个事件的
// 逻辑求值时间；各用自己的合成文档与启用领域集合。只做内存计算，结果由输入确定。
const (
	// windowDays 是默认回放窗口（近90天），可得历史不足时按实际可得截断。
	windowDays = 90
	// maxSamples 是每类代表样本的条数上限。
	maxSamples = 20
)

// DispositionCounts 是处置分布。
type DispositionCounts struct {
	Pursue  int `json:"pursue_candidate"`
	Review  int `json:"review_candidate"`
	Watch   int `json:"watch_signal"`
	Exclude int `json:"excluded"`
	None    int `json:"no_signal"`
}

func (d *DispositionCounts) add(x policy.Disposition) {
	switch x {
	case policy.PursueCandidate:
		d.Pursue++
	case policy.ReviewCandidate:
		d.Review++
	case policy.WatchSignal:
		d.Watch++
	case policy.Excluded:
		d.Exclude++
	default:
		d.None++
	}
}

func (d DispositionCounts) signals() int { return d.Pursue + d.Review + d.Watch }

// WeeklyRates 是平均每周数量。
type WeeklyRates struct {
	Pursue float64 `json:"pursue_candidate"`
	Review float64 `json:"review_candidate"`
	Watch  float64 `json:"watch_signal"`
}

// Sample 是代表样本。Scope 是判断查看权限所需的对象范围（随报告保存，不出现在响应中，见 present）。
type Sample struct {
	FactID     string       `json:"fact_id"`
	FactType   string       `json:"fact_type"`
	OccurredAt time.Time    `json:"occurred_at"`
	Title      string       `json:"title,omitempty"`
	AmountCNY  *int64       `json:"amount_cny,omitempty"`
	Draft      string       `json:"draft"`
	Active     string       `json:"active,omitempty"`
	Scope      *SampleScope `json:"scope,omitempty"`
}

// SampleScope 是样本对应公共事实的信息领域与所在区划（事实视图的 location，未知时为空），用于按查看者判断数据覆盖。
type SampleScope struct {
	InfoDomain string `json:"info_domain"`
	Region     string `json:"region,omitempty"`
}

// Diff 是相对生效版本的差异。*Withheld 只出现在响应中：该列表中当前查看者无权查看、未返回的样本数（见 present）。
type Diff struct {
	Added           int      `json:"added"`
	Removed         int      `json:"removed"`
	Changed         int      `json:"changed"`
	AddedSamples    []Sample `json:"added_samples"`
	RemovedSamples  []Sample `json:"removed_samples"`
	ChangedSamples  []Sample `json:"changed_samples"`
	AddedWithheld   *int     `json:"added_withheld,omitempty"`
	RemovedWithheld *int     `json:"removed_withheld,omitempty"`
	ChangedWithheld *int     `json:"changed_withheld,omitempty"`
}

// UnknownCount 是一种未知原因的计数。
type UnknownCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// RuleStats 是草案一侧一条规则的命中比例。
type RuleStats struct {
	ID             string         `json:"id"`
	Clause         string         `json:"clause"`
	TrueRatio      float64        `json:"true_ratio"`
	FalseRatio     float64        `json:"false_ratio"`
	UnknownRatio   float64        `json:"unknown_ratio"`
	UnknownReasons []UnknownCount `json:"unknown_reasons"`
}

// Hint 是提示项。
type Hint struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Report 是仿真报告（契约 SimulationReport）。
type Report struct {
	Evaluated           int                `json:"evaluated"`
	Distribution        DispositionCounts  `json:"distribution"`
	Weekly              WeeklyRates        `json:"weekly"`
	CompareDistribution *DispositionCounts `json:"compare_distribution,omitempty"`
	Diff                *Diff              `json:"diff,omitempty"`
	Samples             []Sample           `json:"samples,omitempty"`
	SamplesWithheld     *int               `json:"samples_withheld,omitempty"`
	Rules               []RuleStats        `json:"rules"`
	TopUnknownRules     []string           `json:"top_unknown_rules"`
	Hints               []Hint             `json:"hints"`
}

// 未知原因（契约 UnknownReason），按优先级排列：一次求值中规则的未知只计一个原因。
const (
	reasonNotConnected = "data_not_connected"
	reasonEvidence     = "below_evidence_threshold"
	reasonNotLinked    = "not_linked"
	reasonMissing      = "field_missing"
)

var reasonOrder = []string{reasonNotConnected, reasonEvidence, reasonNotLinked, reasonMissing}

// side 是参与回放的一侧。
type side struct {
	compiled *policy.Compiled
	domains  []string
}

// computation 是一次仿真的全部输入。
type computation struct {
	draft       side
	compare     *side
	features    map[string]policy.Feature
	profile     map[string]any
	lists       map[string]any
	slice       *api.DatasetSlice
	from, to    time.Time
	days        int
	mismatches  []string
	disabledReq []string
}

// selectVersions 按逻辑求值时间选取每条事实当时已可得的最新版本；事件发生时还没有任何可得版本的事实不回放。
func selectVersions(facts []api.FactVersion) []api.FactVersion {
	byID := map[string][]api.FactVersion{}
	order := []string{}
	for _, v := range facts {
		if _, ok := byID[v.ID]; !ok {
			order = append(order, v.ID)
		}
		byID[v.ID] = append(byID[v.ID], v)
	}
	out := []api.FactVersion{}
	for _, id := range order {
		versions := byID[id]
		// 事件时间取最早版本的事件时间（更正不改变事件发生的时间）
		at := versions[0].OccurredAt
		for _, v := range versions {
			if v.OccurredAt.Before(at) {
				at = v.OccurredAt
			}
		}
		var chosen *api.FactVersion
		for i := range versions {
			v := versions[i]
			if v.KnownAt.After(at) {
				continue
			}
			if chosen == nil || v.KnownAt.After(chosen.KnownAt) {
				chosen = &versions[i]
			}
		}
		if chosen != nil {
			c := *chosen
			c.OccurredAt = at
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b api.FactVersion) int {
		if c := a.OccurredAt.Compare(b.OccurredAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func clone(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

// setPath 把关联视图项挂到 fact.<路径>（如 winner.credit_flags → fact.winner.credit_flags）。
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

// visible 判断事实对一侧是否可见（事实类型的信息领域在该侧启用；未登记领域的事实类型按可见处理）。
func (c *computation) visible(s side, v api.FactVersion) bool {
	d, ok := c.slice.FactDomains[v.Type]
	return !ok || slices.Contains(s.domains, d)
}

// input 组装一侧的求值输入：该侧未启用领域提供的关联视图项不出现（读取为未知）。
func (c *computation) input(s side, v api.FactVersion) policy.Input {
	fact, _ := clone(v.Fields).(map[string]any)
	if fact == nil {
		fact = map[string]any{}
	}
	for item, value := range v.Items {
		if d, ok := c.slice.ViewDomains[item]; ok && !slices.Contains(s.domains, d) {
			continue
		}
		setPath(fact, item, clone(value))
	}
	evidence := map[string]policy.Evidence{}
	for path, e := range v.Evidence {
		evidence[path] = policy.Evidence{Confidence: e.Confidence, Method: e.Method, Suspect: e.Suspect}
	}
	return policy.Input{FactType: v.Type, Fact: fact, Event: map[string]any{"type": v.Event}, Profile: c.profile, Lists: c.lists,
		Vocab: map[string]any{}, Now: v.OccurredAt, Evidence: evidence}
}

// itemOf 返回字段路径所属的关联视图项（fact.winner.credit_flags.x → winner.credit_flags）。
func (c *computation) itemOf(field string) (string, bool) {
	path := strings.TrimPrefix(field, "fact.")
	for item := range c.slice.ViewDomains {
		if path == item || strings.HasPrefix(path, item+".") {
			return item, true
		}
	}
	return "", false
}

// profileFields 是策略视图中来自客户档案的字段：缺失时（如档案没有我方主体时的 org_ids）规则未知的原因为 not_linked。
var profileFields = []string{"org_ids", "offering_categories", "service_regions", "brands", "entities"}

// classify 给出规则未知的原因（kernel/policy 的轨迹以视图路径给出证据不达标的字段，以键名给出缺失的字段，以特征名给出未知的特征）：
// 所需关联视图项的领域未启用 → data_not_connected；字段存在而证据不达标 → below_evidence_threshold；档案字段（我方主体）缺失 →
// not_linked；其余 → field_missing。
func (c *computation) classify(s side, v api.FactVersion, fields []string) string {
	best := len(reasonOrder) - 1
	rank := func(r string) {
		if i := slices.Index(reasonOrder, r); i < best {
			best = i
		}
	}
	withheld := func(item string) bool {
		d, ok := c.slice.ViewDomains[item]
		return ok && !slices.Contains(s.domains, d) && v.Items[item] != nil
	}
	for _, f := range fields {
		name := strings.TrimPrefix(f, "features.")
		if feature, ok := c.features[name]; ok {
			for _, item := range feature.View {
				if withheld(item) {
					rank(reasonNotConnected)
				}
			}
			continue
		}
		if _, ok := v.Evidence[f]; ok {
			rank(reasonEvidence)
			continue
		}
		if item, ok := c.itemOf(f); ok && withheld(item) {
			rank(reasonNotConnected)
			continue
		}
		key := f[strings.LastIndex(f, ".")+1:]
		for item := range v.Items {
			if (item == key || strings.HasSuffix(item, "."+key)) && withheld(item) {
				rank(reasonNotConnected)
			}
		}
		if _, inProfile := c.profile[key]; !inProfile && slices.Contains(profileFields, key) && (strings.HasPrefix(f, "profile.") || !strings.Contains(f, ".")) {
			rank(reasonNotLinked)
		}
	}
	return reasonOrder[best]
}

type evaluated struct {
	fact          api.FactVersion
	draft, active policy.Disposition
	draftSeen     bool
}

func isSignal(d policy.Disposition) bool {
	return d == policy.PursueCandidate || d == policy.ReviewCandidate || d == policy.WatchSignal
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

func sample(e evaluated, withActive bool, domains map[string]string) Sample {
	s := Sample{FactID: e.fact.ID, FactType: e.fact.Type, OccurredAt: e.fact.OccurredAt.UTC(), Title: e.fact.Title, AmountCNY: e.fact.AmountCNY,
		Draft: string(e.draft)}
	if d := domains[e.fact.Type]; d != "" {
		s.Scope = &SampleScope{InfoDomain: d}
		if region, ok := e.fact.Fields["location"].(string); ok {
			s.Scope.Region = region
		}
	}
	if withActive {
		s.Active = string(e.active)
	}
	return s
}

// pick 按金额倒序（无金额在后）、时间倒序、事实标识确定性抽样。
func pick(list []evaluated, withActive bool, domains map[string]string) []Sample {
	slices.SortStableFunc(list, func(a, b evaluated) int {
		amount := func(e evaluated) int64 {
			if e.fact.AmountCNY == nil {
				return -1
			}
			return *e.fact.AmountCNY
		}
		if c := cmp.Compare(amount(b), amount(a)); c != 0 {
			return c
		}
		if c := b.fact.OccurredAt.Compare(a.fact.OccurredAt); c != 0 {
			return c
		}
		return strings.Compare(a.fact.ID, b.fact.ID)
	})
	out := []Sample{}
	for i, e := range list {
		if i == maxSamples {
			break
		}
		out = append(out, sample(e, withActive, domains))
	}
	return out
}

type ruleCount struct {
	clause  string
	t, f, u int
	reasons map[string]int
}

// run 回放切片中的事件并生成报告。求值错误（成本超限等）的事件按无信号计，不中止仿真。
func (c *computation) run() *Report {
	facts := selectVersions(c.slice.Facts)
	r := &Report{Rules: []RuleStats{}, TopUnknownRules: []string{}, Hints: []Hint{}}
	var compareCounts DispositionCounts
	rules := map[string]*ruleCount{}
	ruleOrder := []string{}
	var all []evaluated
	for _, v := range facts {
		if v.OccurredAt.Before(c.from) || !v.OccurredAt.Before(c.to) {
			continue
		}
		e := evaluated{fact: v, draft: policy.NoSignal, active: policy.NoSignal}
		if c.visible(c.draft, v) {
			e.draftSeen = true
			r.Evaluated++
			res, err := c.draft.compiled.Evaluate(c.input(c.draft, v))
			if err == nil {
				e.draft = res.Disposition
				for _, tr := range res.Trace {
					rc, ok := rules[tr.ID]
					if !ok {
						rc = &ruleCount{clause: string(tr.Clause), reasons: map[string]int{}}
						rules[tr.ID] = rc
						ruleOrder = append(ruleOrder, tr.ID)
					}
					switch tr.Result {
					case policy.True:
						rc.t++
					case policy.False:
						rc.f++
					default:
						rc.u++
						rc.reasons[c.classify(c.draft, v, tr.Unknown)]++
					}
				}
			}
			r.Distribution.add(e.draft)
		}
		if c.compare != nil && c.visible(*c.compare, v) {
			res, err := c.compare.compiled.Evaluate(c.input(*c.compare, v))
			if err == nil {
				e.active = res.Disposition
			}
			compareCounts.add(e.active)
		}
		all = append(all, e)
	}
	weeks := float64(c.days) / 7
	r.Weekly = WeeklyRates{Pursue: round(float64(r.Distribution.Pursue)/weeks, 2), Review: round(float64(r.Distribution.Review)/weeks, 2),
		Watch: round(float64(r.Distribution.Watch)/weeks, 2)}
	if c.compare != nil {
		r.CompareDistribution = &compareCounts
		d := &Diff{}
		var added, removed, changed []evaluated
		for _, e := range all {
			switch {
			case isSignal(e.draft) && !isSignal(e.active):
				added = append(added, e)
			case !isSignal(e.draft) && isSignal(e.active):
				removed = append(removed, e)
			case isSignal(e.draft) && isSignal(e.active) && e.draft != e.active:
				changed = append(changed, e)
			}
		}
		d.Added, d.Removed, d.Changed = len(added), len(removed), len(changed)
		domains := c.slice.FactDomains
		d.AddedSamples, d.RemovedSamples, d.ChangedSamples = pick(added, true, domains), pick(removed, true, domains), pick(changed, true, domains)
		r.Diff = d
	} else {
		var signals []evaluated
		for _, e := range all {
			if isSignal(e.draft) {
				signals = append(signals, e)
			}
		}
		r.Samples = pick(signals, false, c.slice.FactDomains)
	}
	slices.Sort(ruleOrder)
	type unknownRank struct {
		id    string
		ratio float64
	}
	var ranks []unknownRank
	for _, id := range ruleOrder {
		rc := rules[id]
		total := float64(rc.t + rc.f + rc.u)
		st := RuleStats{ID: id, Clause: rc.clause, TrueRatio: round(float64(rc.t)/total, 4), FalseRatio: round(float64(rc.f)/total, 4),
			UnknownRatio: round(float64(rc.u)/total, 4), UnknownReasons: []UnknownCount{}}
		for _, reason := range reasonOrder {
			if n := rc.reasons[reason]; n > 0 {
				st.UnknownReasons = append(st.UnknownReasons, UnknownCount{Reason: reason, Count: n})
			}
		}
		r.Rules = append(r.Rules, st)
		if rc.u > 0 {
			ranks = append(ranks, unknownRank{id, float64(rc.u) / total})
		}
	}
	slices.SortStableFunc(ranks, func(a, b unknownRank) int {
		if c := cmp.Compare(b.ratio, a.ratio); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	for i, k := range ranks {
		if i == 5 {
			break
		}
		r.TopUnknownRules = append(r.TopUnknownRules, k.id)
	}
	r.Hints = c.hints(r)
	return r
}

func (c *computation) hints(r *Report) []Hint {
	out := []Hint{}
	if len(c.mismatches) > 0 {
		out = append(out, Hint{Code: "example_mismatch", Detail: "模板样例结果与期望不一致：" + strings.Join(c.mismatches, "、")})
	}
	if len(c.disabledReq) > 0 {
		out = append(out, Hint{Code: "hard_condition_disabled", Detail: "停用了硬条件：" + strings.Join(c.disabledReq, "、")})
	}
	signals := r.Distribution.signals()
	if signals == 0 {
		out = append(out, Hint{Code: "zero_results", Detail: "窗口内没有产生信号"})
	} else if r.Distribution.Review*2 > signals {
		out = append(out, Hint{Code: "review_majority", Detail: fmt.Sprintf("复核候选占信号的 %d/%d", r.Distribution.Review, signals)})
	}
	if c.days < windowDays {
		out = append(out, Hint{Code: "coverage_insufficient", Detail: fmt.Sprintf("可得历史只有 %d 天，少于 %d 天", c.days, windowDays)})
	}
	var missing []string
	types := c.draft.compiled.Policy.Scope.FactTypes
	if len(types) == 0 {
		types = c.draft.compiled.Policy.FactTypes
	}
	for _, t := range types {
		if d, ok := c.slice.FactDomains[t]; ok && !slices.Contains(c.draft.domains, d) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		out = append(out, Hint{Code: "fact_types_not_connected", Detail: "以下事实类型的信息领域未启用，未回放：" + strings.Join(missing, "、")})
	}
	return out
}
