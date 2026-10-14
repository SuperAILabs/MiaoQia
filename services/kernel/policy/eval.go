package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// Truth 是规则的三值结果（第 4.1 节）。
type Truth int8

const (
	False Truth = iota
	True
	Unknown
)

func (t Truth) String() string { return [...]string{"false", "true", "unknown"}[t] }

// Evidence 是视图字段的证据元数据，键为视图路径（如 fact.winner.org_id）。
type Evidence struct {
	Confidence float64
	Method     string // rule / model / manual
	Suspect    bool   // 质量状态为可疑
}

// Input 是一次求值的输入快照（第 4.3 节）。
type Input struct {
	FactType string
	Fact     map[string]any
	Event    map[string]any
	Profile  map[string]any
	Lists    map[string]any
	Vocab    map[string]any
	Now      time.Time // 逻辑求值时间
	Evidence map[string]Evidence
}

// RuleTrace 是单条规则的求值轨迹。
type RuleTrace struct {
	ID      string
	Clause  Clause
	Result  Truth
	Unknown []string // 导致未知的字段（缺失或证据不达标）
	Error   string   // 非缺失类错误（应在编译期排除；出现即告警）
	Say     string   // 渲染后的业务说明
	Cost    uint64
}

// Result 是一次求值的结果。
type Result struct {
	Disposition Disposition
	Score       int
	Reason      string // excluded 时的原因代码
	NeedsReview bool
	Trace       []RuleTrace
	Features    map[string]any // 特征快照（未知的特征不出现）
	Route       Route
	Cost        uint64   // 本次求值的运行时成本（特征 + 规则）
	Alerts      []string // 需告警的运行时问题
}

// ErrCostExceeded 表示单次求值的运行时成本超过上限：中止该事实的求值，不产生信号，调用方记录并告警。
var ErrCostExceeded = errors.New("policy: 求值成本超过上限")

var (
	missingKey   = regexp.MustCompile(`no such (?:key|attribute)[^:]*: ?(.+)$`)
	sayParam     = regexp.MustCompile(`\{params\.([a-z_][a-z0-9_]*)\}`)
	costExceeded = "cost limit exceeded"
)

type evalState struct {
	c        *Compiled
	vars     map[string]any
	patterns []*cel.AttributePatternType
	failed   []string // 证据不达标的路径
	cost     uint64
	alerts   []string
}

func (s *evalState) run(pr *program) (Truth, ref.Val, []string, string, uint64, error) {
	act, err := cel.PartialVars(s.vars, s.patterns...)
	if err != nil {
		return Unknown, nil, nil, err.Error(), 0, nil
	}
	out, det, err := pr.prg.Eval(act)
	var cost uint64
	if det != nil && det.ActualCost() != nil {
		cost = *det.ActualCost()
	}
	s.cost += cost
	if lim := s.c.Limits.EvaluationCost; lim > 0 && s.cost > lim {
		return Unknown, nil, nil, "", cost, ErrCostExceeded
	}
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, costExceeded) {
			return Unknown, nil, nil, "", cost, ErrCostExceeded
		}
		if m := missingKey.FindStringSubmatch(msg); m != nil {
			return Unknown, nil, []string{m[1]}, "", cost, nil
		}
		return Unknown, nil, nil, msg, cost, nil
	}
	if types.IsUnknown(out) {
		var fields []string
		for _, f := range s.failed {
			if strings.Contains(pr.expr, f) {
				fields = append(fields, f)
			}
		}
		return Unknown, out, fields, "", cost, nil
	}
	if b, ok := out.(types.Bool); ok {
		if b {
			return True, out, nil, "", cost, nil
		}
		return False, out, nil, "", cost, nil
	}
	return Unknown, out, nil, "", cost, nil
}

// Evaluate 对一条事实求值（第 4 节）。返回 ErrCostExceeded 时 Result 含已求值部分的轨迹。
func (c *Compiled) Evaluate(in Input) (*Result, error) {
	p := c.Policy
	res := &Result{Features: map[string]any{}}
	scopeTypes := p.Scope.FactTypes
	if len(scopeTypes) == 0 {
		scopeTypes = p.FactTypes
	}
	if !slices.Contains(scopeTypes, in.FactType) {
		res.Disposition = NoSignal
		return res, nil
	}

	params := c.resolveParams(in.Profile)
	st := &evalState{c: c, vars: map[string]any{
		"fact": norm(in.Fact), "event": norm(in.Event), "profile": norm(in.Profile),
		"lists": norm(in.Lists), "vocab": norm(in.Vocab), "params": params,
		"features": map[string]any{}, "now": in.Now,
	}}
	st.addEvidencePatterns(p.EvidencePolicy, in.Evidence)
	finish := func(err error) (*Result, error) {
		res.Cost, res.Alerts = st.cost, st.alerts
		if res.Route == nil && res.Disposition != "" {
			res.Route = p.Route[string(res.Disposition)]
		}
		return res, err
	}

	if c.scope != nil {
		t, _, _, msg, _, err := st.run(c.scope)
		if err != nil {
			return finish(err)
		}
		if msg != "" {
			st.alerts = append(st.alerts, "scope："+msg)
		}
		if t != True { // 未知按不满足处理
			res.Disposition = NoSignal
			return finish(nil)
		}
	}

	// 特征：只计算规则引用到的特征，本次求值内缓存
	feats := map[string]any{}
	for name, f := range c.features {
		t, out, _, msg, _, err := st.run(&f.program)
		if err != nil {
			return finish(err)
		}
		switch {
		case msg != "":
			st.alerts = append(st.alerts, "features."+name+"："+msg)
		case out != nil && types.IsUnknown(out):
			st.patterns = append(st.patterns, cel.AttributePattern("features").QualString(name))
		case out != nil:
			v := out.Value()
			if f.def.Type == "bool" {
				v = t == True
			}
			feats[name] = v
			res.Features[name] = v
		}
	}
	st.vars["features"] = feats

	res.Trace = make([]RuleTrace, 0, len(c.rules)) // 预分配：byClause 持有元素指针
	byClause := map[Clause][]*RuleTrace{}
	for i := range c.rules {
		r := &c.rules[i]
		t, _, unk, msg, cost, err := st.run(&r.program)
		tr := RuleTrace{ID: r.rule.ID, Clause: r.clause, Result: t, Unknown: unk, Error: msg, Cost: cost, Say: renderSay(r.rule.Say, params)}
		if msg != "" {
			st.alerts = append(st.alerts, r.rule.ID+"："+msg)
		}
		res.Trace = append(res.Trace, tr)
		if err != nil {
			return finish(err)
		}
		byClause[r.clause] = append(byClause[r.clause], &res.Trace[len(res.Trace)-1])
	}
	rules := map[string]*Rule{}
	for i := range c.rules {
		rules[c.rules[i].rule.ID] = c.rules[i].rule
	}

	requires := byClause[ClauseRequire]
	anyFalse := func(ignore []string) bool {
		for _, t := range requires {
			if t.Result == False && !slices.Contains(ignore, t.ID) {
				return true
			}
		}
		return false
	}
	// 3. exclude
	for _, t := range byClause[ClauseExclude] {
		if t.Result == True {
			if anyFalse(nil) {
				res.Disposition = NoSignal
			} else {
				res.Disposition, res.Reason = Excluded, rules[t.ID].Reason
			}
			return finish(nil)
		}
	}
	// 4. watch
	for _, t := range byClause[ClauseWatch] {
		if t.Result == True && !anyFalse(rules[t.ID].IgnoreRequires) {
			res.Disposition = WatchSignal
			return finish(nil)
		}
	}
	// 5. require：为假的硬条件优先于未知条件的 missing 处理。
	for _, t := range requires {
		r := rules[t.ID]
		if t.Result == False {
			if r.OnFail == "excluded" {
				res.Disposition, res.Reason = Excluded, r.Reason
			} else {
				res.Disposition = NoSignal
			}
			return finish(nil)
		}
	}
	for _, t := range requires {
		r := rules[t.ID]
		if t.Result == Unknown {
			switch r.Missing {
			case "reject":
				res.Disposition = NoSignal
				return finish(nil)
			case "ignore":
			default: // review
				res.NeedsReview = true
			}
		}
	}
	// 6. score
	res.Score = p.Thresholds.BaseScore
	for _, t := range byClause[ClauseBoost] {
		if t.Result == True {
			res.Score += rules[t.ID].Weight
		}
	}
	for _, t := range byClause[ClausePenalize] {
		if t.Result == True {
			res.Score -= rules[t.ID].Weight
		}
	}
	// 7. disposition
	switch {
	case res.Score < p.Thresholds.DropBelow:
		res.Disposition = NoSignal
	case res.NeedsReview:
		res.Disposition = ReviewCandidate
	case res.Score >= p.Thresholds.PursueAt:
		res.Disposition = PursueCandidate
	default:
		res.Disposition = ReviewCandidate
	}
	return finish(nil)
}

// addEvidencePatterns 把证据不达标的字段标为未知属性（第 4.1 节）。
func (s *evalState) addEvidencePatterns(ep EvidencePolicy, ev map[string]Evidence) {
	for path, e := range ev {
		req := ep.Default
		if f, ok := ep.Fields[path]; ok {
			merged := f
			if req != nil && len(merged.Methods) == 0 {
				merged.Methods = req.Methods
			}
			req = &merged
		}
		ok := !e.Suspect
		if req != nil {
			ok = ok && e.Confidence >= req.MinConfidence
			if len(req.Methods) > 0 && !slices.Contains(req.Methods, e.Method) {
				ok = false
			}
		}
		if ok {
			continue
		}
		parts := strings.Split(path, ".")
		pat := cel.AttributePattern(parts[0])
		for _, q := range parts[1:] {
			pat = pat.QualString(q)
		}
		s.patterns = append(s.patterns, pat)
		s.failed = append(s.failed, path)
	}
	slices.Sort(s.failed)
}

// resolveParams 取参数值：租户填写 > default_from（客户上下文）> default，并按参数类型归一。
func (c *Compiled) resolveParams(profile map[string]any) map[string]any {
	out := map[string]any{}
	for name, def := range c.Policy.Params {
		v, ok := c.Policy.ParamValues[name]
		if !ok && strings.HasPrefix(def.DefaultFrom, "profile.") {
			v, ok = lookup(profile, strings.TrimPrefix(def.DefaultFrom, "profile."))
		}
		if !ok && def.Default != nil {
			v, ok = def.Default, true
		}
		if !ok {
			continue // 取值缺失：表达式中读取为未知
		}
		if cv, err := coerce(def.Type, v); err == nil {
			out[name] = cv
		}
	}
	return out
}

func lookup(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func coerce(typ string, v any) (any, error) {
	v = norm(v)
	switch {
	case typ == "int":
		switch x := v.(type) {
		case int64:
			return x, nil
		case float64:
			return int64(x), nil
		}
	case typ == "double":
		switch x := v.(type) {
		case int64:
			return float64(x), nil
		case float64:
			return x, nil
		}
	case typ == "bool", typ == "string", strings.HasPrefix(typ, "list<"):
		return v, nil
	}
	return nil, fmt.Errorf("参数类型 %s 与取值 %T 不符", typ, v)
}

// norm 把输入归一为 CEL 友好的类型：整数一律 int64，JSON 数字按是否为整数区分。
func norm(v any) any {
	switch x := v.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normVal(e)
		}
		return out
	}
	return normVal(v)
}

func normVal(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return norm(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normVal(e)
		}
		return out
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

func renderSay(say string, params map[string]any) string {
	return sayParam.ReplaceAllStringFunc(say, func(m string) string {
		name := sayParam.FindStringSubmatch(m)[1]
		if v, ok := params[name]; ok {
			return fmt.Sprint(v)
		}
		return m
	})
}
