package app

import (
	"path/filepath"
	"testing"

	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api/facttest"
)

func packs(t *testing.T) *Packs {
	t.Helper()
	p, err := LoadPacks(filepath.Join("..", "..", "..", "..", "..", "packs"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecideParts(t *testing.T) {
	reasons := packs(t).Feedback["APP-TENDER.A02"]
	if reasons == nil || len(reasons.Irrelevant) != 9 {
		t.Fatalf("%+v", reasons)
	}
	base := func() *current {
		return &current{id: "sig", objective: "obj", disposition: "review_candidate", status: "open", stateVersion: 3, contentVersion: 1}
	}
	irrelevant := Request{Action: "feedback", SeenContent: 1, StateVersion: 3, Feedback: &FeedbackInput{Result: "irrelevant", Reasons: []string{"CATEGORY_MISMATCH"}}, Dismiss: true}
	cases := []struct {
		name    string
		c       func() *current
		req     Request
		code    string
		changed bool
		status  string
	}{
		{"不相关并忽略：两部分都改变", base, irrelevant, "", true, "changed"},
		{"已被同事忽略：处理状态无变化、反馈照常写入", func() *current { c := base(); c.status, c.stateVersion = "dismissed", 4; return c }, irrelevant, "", true, "unchanged"},
		{"已关闭：带反馈的动作处理状态不适用", func() *current { c := base(); c.status = "closed"; return c }, irrelevant, "", true, "not_applicable"},
		{"已关闭：只含处理状态的动作 signal_closed", func() *current { c := base(); c.status = "closed"; return c }, Request{Action: "dismiss", SeenContent: 1, StateVersion: 3}, "signal_closed", false, ""},
		{"已转跟进：只含处理状态的动作不适用", func() *current { c := base(); c.status = "converted"; return c }, Request{Action: "dismiss", SeenContent: 1, StateVersion: 3}, "not_applicable", false, ""},
		{"内容版本已变化", base, Request{Action: "dismiss", SeenContent: 0, StateVersion: 3}, "signal_updated", false, ""},
		{"状态版本不一致且效果未成立：冲突", base, Request{Action: "dismiss", SeenContent: 1, StateVersion: 2}, "conflict", false, ""},
		{"效果已成立：无变化，不校验版本", func() *current { c := base(); c.status = "dismissed"; return c }, Request{Action: "dismiss", SeenContent: 1, StateVersion: 0}, "", false, "unchanged"},
		{"不相关缺原因", base, Request{Action: "feedback", SeenContent: 1, Feedback: &FeedbackInput{Result: "irrelevant"}}, "invalid_feedback", false, ""},
		{"其他须写说明", base, Request{Action: "feedback", SeenContent: 1, Feedback: &FeedbackInput{Result: "irrelevant", Reasons: []string{"OTHER"}}}, "invalid_feedback", false, ""},
		{"未登记的原因", base, Request{Action: "feedback", SeenContent: 1, Feedback: &FeedbackInput{Result: "irrelevant", Reasons: []string{"NOPE"}}}, "invalid_feedback", false, ""},
		{"排除信号不接受相关性反馈", func() *current { c := base(); c.disposition = "excluded"; return c }, irrelevant, "not_applicable", false, ""},
		{"排除信号不能忽略", func() *current { c := base(); c.disposition = "excluded"; return c }, Request{Action: "dismiss", SeenContent: 1, StateVersion: 3}, "not_applicable", false, ""},
		{"排除有误须选原因", func() *current { c := base(); c.disposition = "excluded"; return c }, Request{Action: "feedback", SeenContent: 1, Feedback: &FeedbackInput{Result: "exclusion_wrong"}}, "invalid_feedback", false, ""},
		{"调整处置须写原因", base, Request{Action: "adjust", SeenContent: 1, StateVersion: 3, Manual: "pursue_candidate"}, "invalid_feedback", false, ""},
		{"同一人工处置：无变化", func() *current { c := base(); c.status, c.manual = "triaged", "pursue_candidate"; return c }, Request{Action: "adjust", SeenContent: 1, StateVersion: 0, Manual: "pursue_candidate", Reason: "r"}, "", false, "unchanged"},
		{"状态同为已调整而人工处置不同：不是无变化", func() *current { c := base(); c.status, c.manual = "triaged", "watch_signal"; return c }, Request{Action: "adjust", SeenContent: 1, StateVersion: 2, Manual: "pursue_candidate", Reason: "r"}, "conflict", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, e := decide(tc.c(), tc.req, reasons)
			if tc.code != "" {
				if e == nil || e.Code != tc.code {
					t.Fatalf("期望 %s，得到 %+v %+v", tc.code, e, p)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if p.parts.changed() != tc.changed {
				t.Fatalf("%+v", p.parts)
			}
			if tc.status != "" && (p.parts.Status == nil || p.parts.Status.Change != tc.status) {
				t.Fatalf("%+v", p.parts.Status)
			}
		})
	}
}

func TestCompensate(t *testing.T) {
	before := &Feedback{Result: "relevant", Reasons: []string{"ALREADY_KNOWN"}, BasedOn: 0}
	after := &Feedback{Result: "irrelevant", Reasons: []string{"CATEGORY_MISMATCH"}, BasedOn: 0}
	tg := target{id: "op", signal: "sig", content: 0, parts: Parts{Feedback: &FeedbackPart{Change: "changed", Before: before, After: after, Version: 2},
		Status: &StatusPart{Change: "changed", Before: "triaged", After: "dismissed", Version: 5}}}
	c := &current{status: "dismissed", manual: "pursue_candidate", stateVersion: 5, fb: after, fbVersion: 2}
	p, e := compensate(c, tg)
	if e != nil || !p.fb.equal(before) || p.status != "triaged" || p.manual != "pursue_candidate" || p.parts.Feedback.Version != 3 || p.parts.Status.Version != 6 {
		t.Fatalf("%+v %+v", p, e)
	}
	// 之后本人又改过反馈：不撤销
	c.fbVersion = 3
	if _, e := compensate(c, tg); e == nil || e.Code != "conflict" {
		t.Fatalf("%+v", e)
	}
	c.fbVersion = 2
	c.contentVersion = 1
	if _, e := compensate(c, tg); e == nil || e.Code != "signal_updated" {
		t.Fatalf("%+v", e)
	}
	c.status = "closed"
	if _, e := compensate(c, tg); e == nil || e.Code != "signal_closed" {
		t.Fatalf("%+v", e)
	}
	// 调整处置的撤销：原来没有人工处置时回到待处理
	adj := target{parts: Parts{Manual: &ManualPart{Change: "changed", Before: "none", After: "watch_signal", Version: 2},
		Status: &StatusPart{Change: "changed", Before: "open", After: "triaged", Version: 2}}}
	p, e = compensate(&current{status: "triaged", manual: "watch_signal", stateVersion: 2}, adj)
	if e != nil || p.status != "open" || p.manual != "" {
		t.Fatalf("%+v %+v", p, e)
	}
}

func TestClassifyUnknownReasons(t *testing.T) {
	pk := packs(t)
	var compiled *policy.Compiled
	pack, err := policy.LoadPack(filepath.Join("..", "..", "..", "..", "..", "packs", "application_domains", "tendering"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, issues := policy.Compile(pack.Templates["TPL-TENDER-A02-SUPPLY"], pk.Features, policy.DefaultLimits())
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	unit := facttest.BidResult(facttest.Bid{Notice: "ntc_01JA8ZXXXXXXXXXXXXXXXXXXX1", Result: "bdr_01JA8ZXXXXXXXXXXXXXXXXXXX1", Title: "夹具", Winner: "org_01JA8ZXXXXXXXXXXXXXXXXXXX2",
		Amount: 1_000_000, Location: "330106", Categories: []string{"CAT-PIPE-PE"}, CategoryConfidence: 0.5, WinnerConfidence: 0.5})
	enabled := []string{"INF-ORG", "INF-PROC"} // INF-CRED 未启用
	v := assemble(unit, enabled)
	// 档案已关联本企业主体（与中标方不同），X-SELF 只受中标方识别可靠度影响
	profile := map[string]any{"offering_categories": []any{"CAT-PIPE-PE"}, "service_regions": []any{"330000"}, "brands": []any{}, "org_ids": []any{"org_01JA8ZXXXXXXXXXXXXXXXXXXX9"}}
	evidence := map[string]policy.Evidence{}
	for k, e := range unit.Evidence {
		evidence[k] = policy.Evidence{Confidence: e.Confidence, Method: e.Method}
	}
	res, err := compiled.Evaluate(policy.Input{FactType: unit.FactType, Fact: v.Fact, Event: map[string]any{"type": unit.Event}, Profile: profile,
		Lists: lists, Vocab: map[string]any{}, Evidence: evidence})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tr := range traceOf(compiled, pk.Features, v, profile, res) {
		got[tr.ID] = tr.Result + "/" + tr.UnknownReason
	}
	want := map[string]string{"R-CAT": "unknown/CATEGORY_UNCERTAIN", "R-WINNER": "unknown/BELOW_EVIDENCE", "X-SELF": "unknown/BELOW_EVIDENCE",
		"P-CREDIT": "unknown/DOMAIN_NOT_ENABLED", "B-PERMIT": "false/"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s：得到 %s，期望 %s", id, got[id], w)
		}
	}
	if res.Disposition != policy.ReviewCandidate || !res.NeedsReview {
		t.Fatalf("%+v", res)
	}
	// 档案没有我方主体：policy 只报告缺少 org_ids，X-SELF 未生效（SELF_NOT_LINKED）
	profile = map[string]any{"offering_categories": []any{"CAT-PIPE-PE"}, "service_regions": []any{"330000"}, "brands": []any{}}
	unit2 := unit
	unit2.Evidence = map[string]api.Evidence{}
	v2 := assemble(unit2, enabled)
	res2, err := compiled.Evaluate(policy.Input{FactType: unit2.FactType, Fact: v2.Fact, Event: map[string]any{"type": unit2.Event}, Profile: profile,
		Lists: lists, Vocab: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range traceOf(compiled, pk.Features, v2, profile, res2) {
		if tr.ID == "X-SELF" && (tr.Result != "unknown" || tr.UnknownReason != "SELF_NOT_LINKED") {
			t.Fatalf("%+v", tr)
		}
	}
}
