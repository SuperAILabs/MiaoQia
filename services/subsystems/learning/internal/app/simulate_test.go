package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/policy"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api/datasettest"
)

// 以下用例只使用夹具数据（datasettest，均为虚构）。

var allDomains = []string{"INF-PROC", "INF-INVP", "INF-ORG", "INF-CRED"}

func a02(t *testing.T, coverageDays int, domains []string, compare []string) *computation {
	t.Helper()
	pack, err := policy.LoadPack(filepath.Join("..", "..", "..", "..", "..", "packs", "application_domains", "tendering"))
	if err != nil {
		t.Fatal(err)
	}
	c, issues := policy.Compile(pack.Templates["TPL-TENDER-A02-SUPPLY"], pack.Features, policy.DefaultLimits())
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	from := end.AddDate(0, 0, -coverageDays)
	f := datasettest.New(from)
	f.Add(datasettest.A02(end, "org_me")...)
	slice, err := f.Load(context.Background(), end.AddDate(0, 0, -windowDays), end)
	if err != nil {
		t.Fatal(err)
	}
	if from.Before(end.AddDate(0, 0, -windowDays)) {
		from = end.AddDate(0, 0, -windowDays)
	}
	comp := &computation{draft: side{compiled: c, domains: domains}, features: pack.Features, slice: slice, from: from, to: end,
		days:    int(end.Sub(from).Hours() / 24),
		profile: map[string]any{"org_ids": []any{"org_me"}, "offering_categories": []any{"CAT-PIPE-PE"}, "service_regions": []any{"330000"}, "brands": []any{}},
		lists:   map[string]any{"customers": []any{}, "blocked": []any{}, "competitors": []any{}}}
	if compare != nil {
		comp.compare = &side{compiled: c, domains: compare}
	}
	return comp
}

func dispositions(c *computation, s side) map[string]policy.Disposition {
	out := map[string]policy.Disposition{}
	for _, v := range selectVersions(c.slice.Facts) {
		if !c.visible(s, v) {
			continue
		}
		res, err := s.compiled.Evaluate(c.input(s, v))
		if err != nil {
			panic(err)
		}
		out[v.ID] = res.Disposition
	}
	return out
}

func TestSelectVersionsUsesKnownAtAtEventTime(t *testing.T) {
	c := a02(t, 365, allDomains, nil)
	var got []api.FactVersion
	for _, v := range selectVersions(c.slice.Facts) {
		if v.ID == "FX-A02-08" {
			got = append(got, v)
		}
		if v.ID == "FX-A02-11" {
			t.Fatal("窗口外的事实被回放")
		}
	}
	if len(got) != 1 || *got[0].AmountCNY != 2_000_000 {
		t.Fatalf("更正晚于事件时间，应使用首版：%+v", got)
	}
	// 事件时间之前已可得的多个版本：取已可得的最新版本
	late := got[0]
	first, second := late, late
	first.KnownAt, first.AmountCNY = late.OccurredAt.Add(-2*time.Hour), new(int64)
	second.KnownAt = late.OccurredAt.Add(-time.Hour)
	if sel := selectVersions([]api.FactVersion{second, first}); len(sel) != 1 || *sel[0].AmountCNY != 2_000_000 {
		t.Fatalf("%+v", sel)
	}
	// 事件发生时没有任何可得版本：不回放
	never := late
	never.KnownAt = late.OccurredAt.Add(time.Hour)
	if sel := selectVersions([]api.FactVersion{never}); len(sel) != 0 {
		t.Fatalf("%+v", sel)
	}
}

func TestDispositionsFollowEnabledDomains(t *testing.T) {
	c := a02(t, 365, allDomains, nil)
	want := map[string]policy.Disposition{
		"FX-A02-01": policy.PursueCandidate, "FX-A02-02": policy.ReviewCandidate, "FX-A02-03": policy.WatchSignal,
		"FX-A02-04": policy.ReviewCandidate, "FX-A02-05": policy.NoSignal, "FX-A02-06": policy.Excluded,
		"FX-A02-07": policy.ReviewCandidate, "FX-A02-08": policy.ReviewCandidate, "FX-A02-09": policy.ReviewCandidate,
		"FX-A02-10": policy.ReviewCandidate,
	}
	got := dispositions(c, c.draft)
	for id, d := range want {
		if got[id] != d {
			t.Errorf("%s：%s，期望 %s", id, got[id], d)
		}
	}
	// INF-CRED 与 INF-INVP 未启用：信用规则未知（不按否定处理），施工许可不回放
	partial := side{compiled: c.draft.compiled, domains: []string{"INF-PROC", "INF-ORG"}}
	got = dispositions(c, partial)
	if got["FX-A02-04"] != policy.PursueCandidate {
		t.Errorf("FX-A02-04：%s", got["FX-A02-04"])
	}
	if _, ok := got["FX-A02-07"]; ok {
		t.Error("未启用领域的事实类型被回放")
	}
}

func TestUnknownReasons(t *testing.T) {
	c := a02(t, 365, []string{"INF-PROC", "INF-ORG"}, nil)
	r := c.run()
	reasons := map[string]map[string]int{}
	for _, st := range r.Rules {
		reasons[st.ID] = map[string]int{}
		for _, u := range st.UnknownReasons {
			reasons[st.ID][u.Reason] = u.Count
		}
	}
	if reasons["P-CREDIT"][reasonNotConnected] == 0 || reasons["P-CREDIT"][reasonMissing] != 0 {
		t.Errorf("P-CREDIT：%v", reasons["P-CREDIT"])
	}
	if reasons["R-CAT"][reasonMissing] != 1 {
		t.Errorf("R-CAT：%v", reasons["R-CAT"])
	}
	if reasons["R-WINNER"][reasonEvidence] != 1 {
		t.Errorf("R-WINNER：%v", reasons["R-WINNER"])
	}
	if !slices.Contains(r.TopUnknownRules, "P-CREDIT") || len(r.TopUnknownRules) > 5 {
		t.Errorf("%v", r.TopUnknownRules)
	}
	// 档案没有我方主体：X-SELF 的未知原因为 not_linked
	delete(c.profile, "org_ids")
	r = c.run()
	for _, st := range r.Rules {
		if st.ID == "X-SELF" && (len(st.UnknownReasons) == 0 || st.UnknownReasons[0].Reason != reasonNotLinked) {
			t.Errorf("X-SELF：%+v", st)
		}
	}
}

func TestReportDeterministicAndHints(t *testing.T) {
	c := a02(t, 365, allDomains, []string{"INF-PROC", "INF-ORG"})
	first, _ := json.Marshal(c.run())
	for range 5 {
		again, _ := json.Marshal(c.run())
		if string(again) != string(first) {
			t.Fatalf("相同输入的报告不一致：\n%s\n%s", first, again)
		}
	}
	r := c.run()
	if r.Diff == nil || r.CompareDistribution == nil || r.Samples != nil {
		t.Fatalf("有对比版本时给出差异而不是样本：%+v", r)
	}
	// FX-A02-07 只在草案一侧回放（新增）；FX-A02-04 由跟进候选变为复核候选（变化）
	if r.Diff.Added != 1 || r.Diff.AddedSamples[0].FactID != "FX-A02-07" || r.Diff.Changed != 1 || r.Diff.ChangedSamples[0].FactID != "FX-A02-04" {
		t.Fatalf("%+v", r.Diff)
	}
	codes := func(r *Report) []string {
		var out []string
		for _, h := range r.Hints {
			out = append(out, h.Code)
		}
		return out
	}
	if got := codes(r); !slices.Contains(got, "review_majority") || slices.Contains(got, "coverage_insufficient") {
		t.Errorf("%v", got)
	}
	short := a02(t, 30, []string{"INF-PROC", "INF-ORG"}, nil)
	sr := short.run()
	if got := codes(sr); !slices.Contains(got, "coverage_insufficient") || !slices.Contains(got, "fact_types_not_connected") {
		t.Errorf("%v", got)
	}
	if sr.Evaluated != 8 { // 30 天内的 FX-A02-01..06、-08、-09（-07 的信息领域未启用，不计）
		t.Errorf("evaluated %d", sr.Evaluated)
	}
	if len(sr.Samples) == 0 || sr.Samples[0].FactID != "FX-A02-01" {
		t.Errorf("样本按金额降序：%+v", sr.Samples)
	}
}
