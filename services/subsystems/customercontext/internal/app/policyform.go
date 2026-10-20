package app

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"

	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/internal/domain"
)

// 策略草案的表单与合成（PRD 第7段 F2；策略 DSL 3.3）。表单只覆盖 M2 可调范围：模板参数、非锁定规则启停、加减分权重（1–50）、
// 阈值与各处置的通知受众和方式。合成 = 模板（按目标所用的模板版本）+ 参数值 + 覆盖，结果是 kernel/policy 的完整策略文档，
// 停用的规则不进入文档。表单在保存时归一：规则覆盖只保留与模板不同的项并按编号排序，通知按处置给出生效值。

// RuleOverride 是对一条模板规则的覆盖。Weight 为空表示模板权重。
type RuleOverride struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Weight  *int   `json:"weight,omitempty"`
}

// Notify 是某一处置的通知受众与方式。
type Notify struct {
	Audience []string `json:"audience"`
	Mode     string   `json:"mode"`
}

// Thresholds 是处置阈值。
type Thresholds struct {
	BaseScore int `json:"base_score"`
	PursueAt  int `json:"pursue_at"`
	DropBelow int `json:"drop_below"`
}

// PolicyForm 是草案表单。
type PolicyForm struct {
	Params     map[string]domain.Param `json:"params"`
	Rules      []RuleOverride          `json:"rules"`
	Thresholds Thresholds              `json:"thresholds"`
	Notify     map[string]Notify       `json:"notify"`
}

// RuleView 是合成后的一条规则（含停用的规则），随修订与版本保存。
type RuleView struct {
	ID      string `json:"id"`
	Clause  string `json:"clause"`
	Say     string `json:"say"`
	Enabled bool   `json:"enabled"`
	Locked  bool   `json:"locked"`
	Weight  *int   `json:"weight,omitempty"`
}

// Issue 是校验问题；ExampleMismatch 是模板样例的期望与实际处置。
type Issue struct {
	Where   string `json:"where"`
	Message string `json:"message"`
}

type ExampleMismatch struct {
	Name     string `json:"name"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// notifyDispositions 是可配置通知的处置。
var notifyDispositions = []string{"pursue_candidate", "review_candidate", "watch_signal"}

// audiencePattern 是通知受众：目标负责人、全部参与人或目标范围内的业务角色（不含租户管理员；customer-context.md 第11节）。
var audiencePattern = regexp.MustCompile(`^(objective_owner|objective_members|role:(decision_maker|business_developer|pursuit_lead|research_analyst|domain_expert))$`)

type templateRule struct {
	clause policy.Clause
	rule   policy.Rule
}

func templateRules(t *policy.Policy) []templateRule {
	var out []templateRule
	for _, c := range []struct {
		clause policy.Clause
		rules  []policy.Rule
	}{{policy.ClauseRequire, t.Require}, {policy.ClauseExclude, t.Exclude}, {policy.ClauseWatch, t.Watch}, {policy.ClauseBoost, t.Boost}, {policy.ClausePenalize, t.Penalize}} {
		for _, r := range c.rules {
			out = append(out, templateRule{c.clause, r})
		}
	}
	return out
}

func weighted(c policy.Clause) bool { return c == policy.ClauseBoost || c == policy.ClausePenalize }

// templateNotify 读出模板 route 中各处置的通知设置。
func templateNotify(t *policy.Policy) map[string]Notify {
	out := map[string]Notify{}
	for _, d := range notifyDispositions {
		raw, ok := t.Route[d]["notify"]
		if !ok {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var n Notify
		if json.Unmarshal(b, &n) == nil && len(n.Audience) > 0 && n.Mode != "" {
			out[d] = n
		}
	}
	return out
}

// initialForm 是基于模板缺省与目标参数的表单（首版草案，或没有生效版本时重新起草）。
func initialForm(t *policy.Policy, params map[string]domain.Param) PolicyForm {
	return PolicyForm{Params: params, Rules: []RuleOverride{}, Notify: templateNotify(t),
		Thresholds: Thresholds{BaseScore: t.Thresholds.BaseScore, PursueAt: t.Thresholds.PursueAt, DropBelow: t.Thresholds.DropBelow}}
}

// normalizeForm 按模板校验并归一表单。不能保存的输入返回 422（不保存）：未知规则、改动锁定规则、给非加减分规则设权重、
// 受众或方式不合法；参数按第2段规则校验（keep 中未修改的参数不按当前档案重新校验）。
func normalizeForm(c *domain.Catalog, tpl *policy.Policy, dtpl domain.PolicyTemplate, in PolicyForm, profile domain.Profile, keep map[string]domain.Param) (PolicyForm, error) {
	params, err := domain.ResolveParams(c, dtpl, in.Params, profile, keep)
	if err != nil {
		return PolicyForm{}, err
	}
	rules := map[string]templateRule{}
	for _, r := range templateRules(tpl) {
		rules[r.rule.ID] = r
	}
	out := PolicyForm{Params: params, Rules: []RuleOverride{}, Thresholds: in.Thresholds, Notify: templateNotify(tpl)}
	seen := map[string]bool{}
	for _, o := range in.Rules {
		if seen[o.ID] {
			return PolicyForm{}, failure(400, "invalid_request")
		}
		seen[o.ID] = true
		r, ok := rules[o.ID]
		if !ok {
			return PolicyForm{}, failureDetail(422, "unknown_rule", o.ID)
		}
		if o.Weight != nil && !weighted(r.clause) {
			return PolicyForm{}, failureDetail(422, "invalid_weight", o.ID)
		}
		if o.Weight != nil && (*o.Weight < 1 || *o.Weight > 50) {
			return PolicyForm{}, failureDetail(422, "invalid_weight", o.ID)
		}
		changedWeight := o.Weight != nil && *o.Weight != r.rule.Weight
		if r.rule.Locked && (!o.Enabled || changedWeight) {
			return PolicyForm{}, failureDetail(422, "rule_locked", o.ID)
		}
		if o.Enabled && !changedWeight {
			continue // 与模板相同
		}
		n := RuleOverride{ID: o.ID, Enabled: o.Enabled}
		if changedWeight {
			w := *o.Weight
			n.Weight = &w
		}
		out.Rules = append(out.Rules, n)
	}
	sort.Slice(out.Rules, func(i, j int) bool { return out.Rules[i].ID < out.Rules[j].ID })
	for d, n := range in.Notify {
		if !slices.Contains(notifyDispositions, d) || (n.Mode != "immediate" && n.Mode != "digest") || len(n.Audience) == 0 {
			return PolicyForm{}, failureDetail(422, "invalid_notify", d)
		}
		audience := slices.Clone(n.Audience)
		for _, a := range audience {
			if !audiencePattern.MatchString(a) {
				return PolicyForm{}, failureDetail(422, "invalid_notify", a)
			}
		}
		slices.Sort(audience)
		if len(slices.Compact(slices.Clone(audience))) != len(audience) {
			return PolicyForm{}, failureDetail(422, "invalid_notify", d)
		}
		out.Notify[d] = Notify{Audience: audience, Mode: n.Mode}
	}
	return out, nil
}

// ruleViews 给出表单在模板上的逐条规则（含停用的规则）。
func ruleViews(tpl *policy.Policy, form PolicyForm) []RuleView {
	over := map[string]RuleOverride{}
	for _, o := range form.Rules {
		over[o.ID] = o
	}
	var out []RuleView
	for _, r := range templateRules(tpl) {
		v := RuleView{ID: r.rule.ID, Clause: string(r.clause), Say: r.rule.Say, Enabled: true, Locked: r.rule.Locked}
		if weighted(r.clause) {
			w := r.rule.Weight
			v.Weight = &w
		}
		if o, ok := over[r.rule.ID]; ok {
			v.Enabled = o.Enabled
			if o.Weight != nil {
				w := *o.Weight
				v.Weight = &w
			}
		}
		out = append(out, v)
	}
	return out
}

// compose 合成完整策略文档：复制模板，写入本目标参数值，去掉停用的规则，应用权重、阈值与通知覆盖。
// 随档案与取模板默认值的参数不写入参数值，求值时按 default_from / default 解析。
func compose(tpl *policy.Policy, form PolicyForm) (*policy.Policy, error) {
	b, err := json.Marshal(tpl)
	if err != nil {
		return nil, err
	}
	var p policy.Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	p.ParamValues = map[string]any{}
	for name, v := range form.Params {
		if v.Source != domain.FromObjective {
			continue
		}
		if v.Number != nil {
			p.ParamValues[name] = *v.Number
		} else {
			values := make([]any, 0, len(v.Values))
			for _, s := range v.Values {
				values = append(values, s)
			}
			p.ParamValues[name] = values
		}
	}
	over := map[string]RuleOverride{}
	for _, o := range form.Rules {
		over[o.ID] = o
	}
	apply := func(rules []policy.Rule) []policy.Rule {
		out := []policy.Rule{}
		for _, r := range rules {
			if o, ok := over[r.ID]; ok {
				if !o.Enabled {
					continue
				}
				if o.Weight != nil {
					r.Weight = *o.Weight
				}
			}
			out = append(out, r)
		}
		return out
	}
	p.Require, p.Exclude, p.Watch, p.Boost, p.Penalize = apply(p.Require), apply(p.Exclude), apply(p.Watch), apply(p.Boost), apply(p.Penalize)
	p.Thresholds = policy.Thresholds{BaseScore: form.Thresholds.BaseScore, PursueAt: form.Thresholds.PursueAt, DropBelow: form.Thresholds.DropBelow}
	if p.Route == nil {
		p.Route = map[string]policy.Route{}
	}
	for d, n := range form.Notify {
		route := policy.Route{}
		for k, v := range p.Route[d] {
			route[k] = v
		}
		route["notify"] = map[string]any{"audience": n.Audience, "mode": n.Mode}
		p.Route[d] = route
	}
	return &p, nil
}

// validate 编译合成文档并运行模板样例：编译问题与阈值问题使修订不通过校验（照常保存）；样例不一致只列出、不阻止保存。
func validate(doc *policy.Policy, features map[string]policy.Feature) (bool, []Issue, []ExampleMismatch) {
	issues := []Issue{}
	mismatches := []ExampleMismatch{}
	if doc.Thresholds.DropBelow >= doc.Thresholds.PursueAt {
		issues = append(issues, Issue{Where: "thresholds", Message: fmt.Sprintf("drop_below（%d）须小于 pursue_at（%d）", doc.Thresholds.DropBelow, doc.Thresholds.PursueAt)})
	}
	compiled, problems := policy.Compile(doc, features, policy.DefaultLimits())
	for _, p := range problems {
		issues = append(issues, Issue{Where: p.Where, Message: p.Msg})
	}
	if compiled == nil {
		return false, issues, mismatches
	}
	for _, ex := range doc.Examples {
		in, err := doc.ExampleInput(ex)
		if err != nil {
			issues = append(issues, Issue{Where: "examples", Message: err.Error()})
			continue
		}
		res, err := compiled.Evaluate(in)
		if err != nil {
			issues = append(issues, Issue{Where: "examples", Message: fmt.Sprintf("样例「%s」求值失败：%v", ex.Name, err)})
			continue
		}
		if res.Disposition != ex.Expect {
			mismatches = append(mismatches, ExampleMismatch{Name: ex.Name, Expected: string(ex.Expect), Actual: string(res.Disposition)})
		}
	}
	return len(issues) == 0, issues, mismatches
}

// PolicyRuleChange 是规则级差异的一项。
type PolicyRuleChange struct {
	ID         string `json:"id"`
	Clause     string `json:"clause"`
	Change     string `json:"change"`
	FromWeight *int   `json:"from_weight,omitempty"`
	ToWeight   *int   `json:"to_weight,omitempty"`
	Highlight  bool   `json:"highlight"`
}

// ParamChange、ThresholdChange、NotifyChange 是参数、阈值与通知的差异。
type ParamChange struct {
	Name string        `json:"name"`
	From *domain.Param `json:"from,omitempty"`
	To   *domain.Param `json:"to,omitempty"`
}

type ThresholdChange struct {
	Name string `json:"name"`
	From int    `json:"from"`
	To   int    `json:"to"`
}

type NotifyChange struct {
	Disposition string  `json:"disposition"`
	From        *Notify `json:"from,omitempty"`
	To          *Notify `json:"to,omitempty"`
}

// PolicyDiff 是两个策略状态（版本或草案）的规则级差异。
type PolicyDiff struct {
	From           string             `json:"from"`
	To             string             `json:"to"`
	TemplateFrom   string             `json:"template_version_from"`
	TemplateTo     string             `json:"template_version_to"`
	Rules          []PolicyRuleChange `json:"rules"`
	Params         []ParamChange      `json:"params"`
	Thresholds     []ThresholdChange  `json:"thresholds"`
	Notify         []NotifyChange     `json:"notify"`
	DomainsAdded   []string           `json:"domains_added"`
	DomainsRemoved []string           `json:"domains_removed"`
}

// policySide 是参与比较的一侧。
type policySide struct {
	ref, templateVersion string
	form                 PolicyForm
	rules                []RuleView
	domains              []string
}

func diffPolicies(a, b policySide) PolicyDiff {
	d := PolicyDiff{From: a.ref, To: b.ref, TemplateFrom: a.templateVersion, TemplateTo: b.templateVersion,
		Rules: []PolicyRuleChange{}, Params: []ParamChange{}, Thresholds: []ThresholdChange{}, Notify: []NotifyChange{}, DomainsAdded: []string{}, DomainsRemoved: []string{}}
	ar := map[string]RuleView{}
	for _, r := range a.rules {
		ar[r.ID] = r
	}
	br := map[string]RuleView{}
	for _, r := range b.rules {
		br[r.ID] = r
	}
	for _, r := range a.rules {
		if _, ok := br[r.ID]; !ok {
			d.Rules = append(d.Rules, PolicyRuleChange{ID: r.ID, Clause: r.Clause, Change: "removed", Highlight: r.Clause == "require" && r.Enabled})
		}
	}
	for _, r := range b.rules {
		old, ok := ar[r.ID]
		switch {
		case !ok:
			d.Rules = append(d.Rules, PolicyRuleChange{ID: r.ID, Clause: r.Clause, Change: "added"})
		case old.Enabled && !r.Enabled:
			d.Rules = append(d.Rules, PolicyRuleChange{ID: r.ID, Clause: r.Clause, Change: "disabled", Highlight: r.Clause == "require"})
		case !old.Enabled && r.Enabled:
			d.Rules = append(d.Rules, PolicyRuleChange{ID: r.ID, Clause: r.Clause, Change: "enabled"})
		}
		if ok && old.Weight != nil && r.Weight != nil && *old.Weight != *r.Weight {
			d.Rules = append(d.Rules, PolicyRuleChange{ID: r.ID, Clause: r.Clause, Change: "weight_changed", FromWeight: old.Weight, ToWeight: r.Weight})
		}
	}
	names := map[string]bool{}
	for n := range a.form.Params {
		names[n] = true
	}
	for n := range b.form.Params {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		pa, oka := a.form.Params[n]
		pb, okb := b.form.Params[n]
		if oka && okb && reflect.DeepEqual(pa, pb) {
			continue
		}
		c := ParamChange{Name: n}
		if oka {
			c.From = &pa
		}
		if okb {
			c.To = &pb
		}
		d.Params = append(d.Params, c)
	}
	for _, t := range []struct {
		name     string
		from, to int
	}{{"base_score", a.form.Thresholds.BaseScore, b.form.Thresholds.BaseScore}, {"pursue_at", a.form.Thresholds.PursueAt, b.form.Thresholds.PursueAt},
		{"drop_below", a.form.Thresholds.DropBelow, b.form.Thresholds.DropBelow}} {
		if t.from != t.to {
			d.Thresholds = append(d.Thresholds, ThresholdChange{Name: t.name, From: t.from, To: t.to})
		}
	}
	for _, disp := range notifyDispositions {
		na, oka := a.form.Notify[disp]
		nb, okb := b.form.Notify[disp]
		if oka == okb && reflect.DeepEqual(na, nb) {
			continue
		}
		c := NotifyChange{Disposition: disp}
		if oka {
			c.From = &na
		}
		if okb {
			c.To = &nb
		}
		d.Notify = append(d.Notify, c)
	}
	for _, x := range b.domains {
		if !slices.Contains(a.domains, x) {
			d.DomainsAdded = append(d.DomainsAdded, x)
		}
	}
	for _, x := range a.domains {
		if !slices.Contains(b.domains, x) {
			d.DomainsRemoved = append(d.DomainsRemoved, x)
		}
	}
	return d
}
