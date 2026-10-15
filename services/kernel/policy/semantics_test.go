package policy

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func mustCompile(t *testing.T, p *Policy, lim Limits) *Compiled {
	t.Helper()
	c, issues := Compile(p, nil, lim)
	if issues != nil {
		t.Fatalf("编译失败：%v", issues)
	}
	return c
}

func basePolicy() *Policy {
	return &Policy{
		ID: "TPL-TEST", Scope: Scope{FactTypes: []string{"tendering.bid_result"}},
		Require: []Rule{{ID: "R-WINNER", When: "has(fact.winner.org_id)", Missing: "review"}},
		Boost:   []Rule{{ID: "B-AMOUNT", When: "money.cny(fact.amount) >= 1000000.0", Weight: 30}},
		EvidencePolicy: EvidencePolicy{
			Default: &FieldEvidence{MinConfidence: 0.8, Methods: []string{"rule", "model", "manual"}},
			Fields:  map[string]FieldEvidence{"fact.winner.org_id": {MinConfidence: 0.9}},
		},
		Thresholds: Thresholds{BaseScore: 50, PursueAt: 70, DropBelow: 20},
	}
}

func input(fact map[string]any) Input {
	return Input{FactType: "tendering.bid_result", Fact: fact, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
}

func TestEvidenceBelowThresholdIsUnknown(t *testing.T) {
	c := mustCompile(t, basePolicy(), DefaultLimits())
	fact := map[string]any{"winner": map[string]any{"org_id": "org_1"}, "amount": map[string]any{"value": "2000000", "currency": "CNY"}}

	r, _ := c.Evaluate(input(fact))
	if r.Disposition != PursueCandidate {
		t.Fatalf("证据达标时期望 pursue_candidate，得到 %s", r.Disposition)
	}
	in := input(fact)
	in.Evidence = map[string]Evidence{"fact.winner.org_id": {Confidence: 0.85, Method: "model"}}
	r, _ = c.Evaluate(in)
	if r.Trace[0].Result != Unknown || r.Disposition != ReviewCandidate {
		t.Fatalf("主体置信度低于字段门槛时 R-WINNER 应为未知并需复核，得到 %s / %s", r.Trace[0].Result, r.Disposition)
	}
	in.Evidence = map[string]Evidence{"fact.amount": {Confidence: 0.99, Method: "guess"}}
	r, _ = c.Evaluate(in)
	if r.Trace[1].Result != Unknown || r.Score != 50 {
		t.Fatalf("方法不在允许列表时金额加分应为未知且不计分，得到 %s / %d", r.Trace[1].Result, r.Score)
	}
}

func TestDispositionOrder(t *testing.T) {
	p := basePolicy()
	p.Require = append(p.Require, Rule{ID: "R-REGION", When: `region.within(fact.location, ["330000"])`, Missing: "review"})
	p.Exclude = []Rule{{ID: "X-BLOCKED", When: `fact.winner.org_id in lists.blocked`, Reason: "BLOCKED_PARTNER"}}
	p.Watch = []Rule{{ID: "W-CAND", When: `fact.result_type == "candidate_published"`}}
	c := mustCompile(t, p, DefaultLimits())
	lists := map[string]any{"blocked": []any{"org_1"}}
	cases := []struct {
		name string
		fact map[string]any
		want Disposition
	}{
		{"排除且硬条件不为假", map[string]any{"winner": map[string]any{"org_id": "org_1"}, "location": "330106"}, Excluded},
		{"排除但区域为假：不产生排除记录", map[string]any{"winner": map[string]any{"org_id": "org_1"}, "location": "440300"}, NoSignal},
		{"观察受硬条件约束", map[string]any{"winner": map[string]any{"org_id": "org_2"}, "location": "440300", "result_type": "candidate_published"}, NoSignal},
		{"观察：缺少字段不阻止", map[string]any{"location": "330106", "result_type": "candidate_published"}, WatchSignal},
		{"区域缺失需复核", map[string]any{"winner": map[string]any{"org_id": "org_2"}}, ReviewCandidate},
	}
	for _, tc := range cases {
		in := input(tc.fact)
		in.Lists = lists
		r, err := c.Evaluate(in)
		if err != nil || r.Disposition != tc.want {
			t.Errorf("%s：期望 %s，得到 %v（%v）\n%s", tc.name, tc.want, r.Disposition, err, traceText(r))
		}
	}
	r, _ := c.Evaluate(Input{FactType: "org.change"})
	if r.Disposition != NoSignal {
		t.Errorf("范围外的事实类型应为 no_signal")
	}
}

func TestOnFailExcluded(t *testing.T) {
	p := basePolicy()
	p.Require[0] = Rule{ID: "R-WINNER", When: `fact.winner.org_id != "org_me"`, OnFail: "excluded", Reason: "SELF_IS_WINNER"}
	c := mustCompile(t, p, DefaultLimits())
	r, _ := c.Evaluate(input(map[string]any{"winner": map[string]any{"org_id": "org_me"}}))
	if r.Disposition != Excluded || r.Reason != "SELF_IS_WINNER" {
		t.Fatalf("期望 excluded / SELF_IS_WINNER，得到 %s / %s", r.Disposition, r.Reason)
	}
	p.Require[0].Reason = ""
	if _, issues := Compile(p, nil, DefaultLimits()); issues == nil {
		t.Fatal("on_fail: excluded 缺少 reason 应编译失败")
	}
}

func TestFalseRequirePrecedesUnknown(t *testing.T) {
	for _, missing := range []string{"reject", "review", "ignore"} {
		for _, onFail := range []string{"excluded", "no_signal"} {
			for _, falseFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/falseFirst=%t", missing, onFail, falseFirst), func(t *testing.T) {
					p := basePolicy()
					p.Require = []Rule{
						{ID: "R-UNKNOWN", When: "fact.missing > 0", Missing: missing},
						{ID: "R-FALSE", When: "false", OnFail: onFail, Reason: "SELF_IS_WINNER"},
					}
					if falseFirst {
						p.Require[0], p.Require[1] = p.Require[1], p.Require[0]
					}
					r, err := mustCompile(t, p, DefaultLimits()).Evaluate(input(nil))
					if err != nil {
						t.Fatal(err)
					}
					want, reason := NoSignal, ""
					if onFail == "excluded" {
						want, reason = Excluded, "SELF_IS_WINNER"
					}
					if r.Disposition != want || r.Reason != reason || r.NeedsReview {
						t.Fatalf("false require should take precedence: got %s / %s / review=%t", r.Disposition, r.Reason, r.NeedsReview)
					}
				})
			}
		}
	}
}

func TestCompileRejects(t *testing.T) {
	cases := map[string]string{
		"非布尔结果":  `"abc"`,
		"未声明变量":  `tenant.id == "t1"`,
		"静态成本超限": `fact.items.exists(i, i.categories.exists(c, c.code in lists.customers))`,
		"语法错误":   `fact.amount >`,
	}
	for name, expr := range cases {
		p := basePolicy()
		p.Boost = []Rule{{ID: "B-X", When: expr, Weight: 1}}
		if _, issues := Compile(p, nil, DefaultLimits()); issues == nil {
			t.Errorf("%s：期望编译失败", name)
		}
	}
	p := basePolicy()
	for i := 0; i < MaxRulesPerPolicy; i++ {
		p.Boost = append(p.Boost, Rule{ID: "B-" + string(rune('A'+i%26)) + string(rune('A'+i/26)), When: "true", Weight: 1})
	}
	if _, issues := Compile(p, nil, DefaultLimits()); issues == nil {
		t.Error("规则数超过上限应编译失败")
	}
}

func TestRuntimeCostLimit(t *testing.T) {
	p := basePolicy()
	p.Boost = []Rule{{ID: "B-OVERLAP", When: "sets.overlaps(fact.org_ids, lists.customers)", Weight: 1}}
	c := mustCompile(t, p, Limits{EvaluationCost: 1000})
	ids := make([]any, 5000)
	for i := range ids {
		ids[i] = "org_" + string(rune(i))
	}
	in := input(map[string]any{"winner": map[string]any{"org_id": "o"}, "org_ids": []any{"x"}})
	in.Lists = map[string]any{"customers": ids}
	if _, err := c.Evaluate(in); !errors.Is(err, ErrCostExceeded) {
		t.Fatalf("期望 ErrCostExceeded，得到 %v", err)
	}
}

func TestPlatformFunctions(t *testing.T) {
	cases := map[string]bool{
		`region.within("330106", ["330000"])`: true,
		`region.within("330106", ["330100"])`: true,
		`region.within("330106", ["330200"])`: false,
		`region.within("440300", ["000000"])`: true,
		`category.any_match([{"categories": [{"scheme": "industry:building-materials", "code": "CAT-PIPE-PE-100"}]}], ["CAT-PIPE"])`: true,
		`category.any_match([{"categories": [{"scheme": "industry:building-materials", "code": "CAT-PIPELINE"}]}], ["CAT-PIPE"])`:    false,
		`category.any_match([{"categories": [{"scheme": "industry:building-materials", "code": "CAT-PIPE-PE"}]}], ["CAT-PIPE"])`:     true,
		`category.any_match([{"categories": [{"scheme": "gov-procurement-catalog", "code": "CAT-PIPE-PE"}]}], ["CAT-PIPE"])`:         false,
		`category.any_match([{"scheme": "industry:building-materials", "code": "CAT-PIPE-PE"}], ["CAT-PIPE"])`:                       true,
		`category.any_match([{"scheme": "industry:it", "code": "CAT-IT-SW"}], ["CAT-PIPE"])`:                                         false,
		`category.any_match([{"categories": [{"code": "CAT-PIPE-PE"}]}], ["CAT-PIPE"])`:                                              false,
		`category.any_match([{"code": "CAT-PIPE-PE"}], ["CAT-PIPE"])`:                                                                false,
		`text.contains_any("市政管网改造", ["管网", "道路"])`:                                                                                  true,
		`time.days_between("2026-11-02T00:00:00Z", "2026-11-01T12:00:00Z") == -1`:                                                    true,
		`time.days_between(now, "2026-11-20T09:30:00+08:00") == 19`:                                                                  true,
		`money.cny({"value": "120.5", "currency": "CNY"}) == 120.5`:                                                                  true,
		`sets.overlaps(["a", "b"], ["c", "b"])`:                                                                                      true,
	}
	for expr, want := range cases {
		p := basePolicy()
		p.Require = nil
		p.Boost = []Rule{{ID: "B-X", When: expr, Weight: 1}}
		c := mustCompile(t, p, DefaultLimits())
		r, _ := c.Evaluate(input(nil))
		if got := r.Trace[0].Result; got != map[bool]Truth{true: True, false: False}[want] {
			t.Errorf("%s：期望 %v，得到 %s %s", expr, want, got, r.Trace[0].Error)
		}
	}
	p := basePolicy()
	p.Require = nil
	p.Boost = []Rule{{ID: "B-USD", When: `money.cny({"value": "1", "currency": "USD"}) > 0.0`, Weight: 1}}
	r, _ := mustCompile(t, p, DefaultLimits()).Evaluate(input(nil))
	if r.Trace[0].Result != Unknown {
		t.Errorf("非人民币金额应为未知")
	}
}

// R1 模板在规模上界下不超过限制（防止模板或函数修改造成回归）。
func TestR1WithinLimits(t *testing.T) {
	for _, r := range measureCosts(t) {
		if r.maxStatic > MaxExpressionStaticCost || r.worstCost > MaxEvaluationCost {
			t.Errorf("%s 超出限制：静态 %d，运行时 %d", r.template, r.maxStatic, r.worstCost)
		}
	}
}

// 编译产物可被多个 goroutine 并发求值。
func TestConcurrentEvaluate(t *testing.T) {
	pack := loadTendering(t)
	tpl := pack.Templates["TPL-TENDER-A02-SUPPLY"]
	c, issues := Compile(tpl, pack.Features, DefaultLimits())
	if issues != nil {
		t.Fatal(issues)
	}
	in, _ := tpl.ExampleInput(tpl.Examples[0])
	done := make(chan Disposition)
	for range 8 {
		go func() {
			var d Disposition
			for range 50 {
				r, _ := c.Evaluate(in)
				d = r.Disposition
			}
			done <- d
		}()
	}
	for range 8 {
		if d := <-done; d != tpl.Examples[0].Expect {
			t.Fatalf("并发求值结果不一致：%s", d)
		}
	}
}

func TestParamValues(t *testing.T) {
	pack := loadTendering(t)
	tpl := *pack.Templates["TPL-TENDER-A01-BID"]
	tpl.ParamValues = map[string]any{"min_prep_days": 0}
	if _, issues := Compile(&tpl, pack.Features, DefaultLimits()); issues == nil {
		t.Error("低于参数下限应编译失败")
	}
	tpl.ParamValues = map[string]any{"unknown_param": 1}
	if _, issues := Compile(&tpl, pack.Features, DefaultLimits()); issues == nil {
		t.Error("未声明的参数应编译失败")
	}
	tpl.ParamValues = map[string]any{"min_prep_days": 25}
	c, issues := Compile(&tpl, pack.Features, DefaultLimits())
	if issues != nil {
		t.Fatal(issues)
	}
	in, _ := tpl.ExampleInput(tpl.Examples[0]) // 距截止 19 天
	r, _ := c.Evaluate(in)
	if r.Disposition != NoSignal || r.Trace[2].Say != "距投标截止不少于 25 天" {
		t.Errorf("租户参数应覆盖默认值：%s / %q", r.Disposition, r.Trace[2].Say)
	}
}

func TestLowConfidenceCategoryNeedsReview(t *testing.T) {
	pack := loadTendering(t)
	tpl := pack.Templates["TPL-TENDER-A01-BID"]
	c, issues := Compile(tpl, pack.Features, DefaultLimits())
	if issues != nil {
		t.Fatal(issues)
	}
	in, _ := tpl.ExampleInput(tpl.Examples[0])
	in.Evidence = map[string]Evidence{"fact.categories": {Confidence: 0.6, Method: "model"}}
	r, _ := c.Evaluate(in)
	if r.Disposition != ReviewCandidate || r.Trace[0].ID != "R-CAT" || r.Trace[0].Result != Unknown {
		t.Errorf("增强品类置信度不足应转人工复核：%s / %s=%s", r.Disposition, r.Trace[0].ID, r.Trace[0].Result)
	}
	in.Evidence = map[string]Evidence{"fact.categories": {Confidence: 0.9, Method: "model"}}
	if r, _ := c.Evaluate(in); r.Disposition != tpl.Examples[0].Expect {
		t.Errorf("增强品类置信度达标应与样例一致：%s", r.Disposition)
	}
}
