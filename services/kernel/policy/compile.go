package policy

import (
	"fmt"
	"regexp"
	"sort"

	"cel.dev/cel-go/cel"
)

// Limits 是编译与求值的限制；零值表示不限制。
type Limits struct {
	RulesPerPolicy       int
	ExpressionLength     int
	ExpressionStaticCost uint64 // 单条表达式的静态成本上界
	EvaluationCost       uint64 // 单次事实求值（特征 + 全部规则）的运行时成本上限
}

// DefaultLimits 是 contracts/policy/cel-environment.yaml 中的限制。
func DefaultLimits() Limits {
	return Limits{
		RulesPerPolicy:       MaxRulesPerPolicy,
		ExpressionLength:     MaxExpressionLength,
		ExpressionStaticCost: MaxExpressionStaticCost,
		EvaluationCost:       MaxEvaluationCost,
	}
}

// Issue 是编译发现的问题。
type Issue struct {
	Where string // 规则或特征编号
	Msg   string
}

func (i Issue) String() string { return i.Where + "：" + i.Msg }

type program struct {
	expr       string
	prg        cel.Program
	staticCost uint64
}

type compiledRule struct {
	clause Clause
	rule   *Rule
	program
}

// Compiled 是编译后的策略，可并发求值。
type Compiled struct {
	Policy   *Policy
	Limits   Limits
	scope    *program
	rules    []compiledRule
	features map[string]compiledFeature // 只含规则引用到的特征
	// MaxStaticCost 是各表达式静态成本上界的最大值，供基准测试与审阅。
	MaxStaticCost uint64
}

type compiledFeature struct {
	def Feature
	program
}

var featureRef = regexp.MustCompile(`\bfeatures\.([a-z_][a-z0-9_]*)`)

// Compile 校验并编译策略（第 6 节：结构、类型、成本检查）。下推编译由 search 侧的编译器完成，不在本包。
func Compile(p *Policy, features map[string]Feature, lim Limits) (*Compiled, []Issue) {
	env, err := NewEnv()
	if err != nil {
		return nil, []Issue{{"env", err.Error()}}
	}
	c := &Compiled{Policy: p, Limits: lim, features: map[string]compiledFeature{}}
	var issues []Issue
	add := func(where, format string, a ...any) { issues = append(issues, Issue{where, fmt.Sprintf(format, a...)}) }

	compile := func(where, expr string, wantBool bool) *program {
		if lim.ExpressionLength > 0 && len([]rune(expr)) > lim.ExpressionLength {
			add(where, "表达式长度 %d 超过上限 %d", len([]rune(expr)), lim.ExpressionLength)
			return nil
		}
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			add(where, "%v", iss.Err())
			return nil
		}
		if wantBool && ast.OutputType() != cel.BoolType && ast.OutputType() != cel.DynType {
			add(where, "结果类型为 %s，必须为布尔", ast.OutputType())
			return nil
		}
		est, err := env.EstimateCost(ast, staticEstimator{})
		if err != nil {
			add(where, "成本估算失败：%v", err)
			return nil
		}
		if lim.ExpressionStaticCost > 0 && est.Max > lim.ExpressionStaticCost {
			add(where, "静态成本上界 %d 超过上限 %d", est.Max, lim.ExpressionStaticCost)
			return nil
		}
		opts := []cel.ProgramOption{cel.EvalOptions(cel.OptPartialEval), cel.CostTracking(runtimeEstimator{})}
		if lim.EvaluationCost > 0 {
			opts = append(opts, cel.CostLimit(lim.EvaluationCost))
		}
		prg, err := env.Program(ast, opts...)
		if err != nil {
			add(where, "%v", err)
			return nil
		}
		if est.Max > c.MaxStaticCost {
			c.MaxStaticCost = est.Max
		}
		return &program{expr: expr, prg: prg, staticCost: est.Max}
	}

	for name, v := range p.ParamValues {
		def, ok := p.Params[name]
		if !ok {
			add("params."+name, "模板未声明该参数")
			continue
		}
		cv, err := coerce(def.Type, v)
		if err != nil {
			add("params."+name, "%v", err)
			continue
		}
		if f, ok := asFloat(cv); ok && ((def.Min != nil && f < *def.Min) || (def.Max != nil && f > *def.Max)) {
			add("params."+name, "取值 %v 超出范围", cv)
		}
	}

	all := p.rules()
	if lim.RulesPerPolicy > 0 && len(all) > lim.RulesPerPolicy {
		add("policy", "规则数 %d 超过上限 %d", len(all), lim.RulesPerPolicy)
	}
	if p.Scope.When != "" {
		c.scope = compile("scope", p.Scope.When, true)
	}
	seen := map[string]bool{}
	used := map[string]bool{}
	for _, r := range all {
		if seen[r.Rule.ID] {
			add(r.Rule.ID, "规则编号重复")
		}
		seen[r.Rule.ID] = true
		if r.Clause == ClauseExclude && r.Rule.Reason == "" {
			add(r.Rule.ID, "排除规则缺少 reason")
		}
		if r.Clause == ClauseRequire && r.Rule.OnFail == "excluded" && r.Rule.Reason == "" {
			add(r.Rule.ID, "on_fail: excluded 缺少 reason")
		}
		for _, m := range featureRef.FindAllStringSubmatch(r.Rule.When, -1) {
			used[m[1]] = true
		}
		if pr := compile(r.Rule.ID, r.Rule.When, true); pr != nil {
			c.rules = append(c.rules, compiledRule{clause: r.Clause, rule: r.Rule, program: *pr})
		}
	}
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		def, ok := features[n]
		if !ok {
			add("features."+n, "特征未声明")
			continue
		}
		if featureRef.MatchString(def.Expr) {
			add("features."+n, "特征不能引用其他特征")
			continue
		}
		if pr := compile("features."+n, def.Expr, def.Type == "bool"); pr != nil {
			c.features[n] = compiledFeature{def: def, program: *pr}
		}
	}
	if len(issues) > 0 {
		return nil, issues
	}
	return c, nil
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
