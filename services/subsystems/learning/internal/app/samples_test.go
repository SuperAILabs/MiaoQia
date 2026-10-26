package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	commerce "github.com/superailabs/miaoqia/services/subsystems/commerce/api"
)

type fakeDecider struct {
	deny map[string]bool // 无 can_search_public 的主体
	err  error
	n    int
}

func (f *fakeDecider) Check(_ context.Context, req authz.Request) (bool, error) {
	f.n++
	if req.Relation != "can_search_public" || !strings.HasPrefix(req.Object, "tenant:") {
		return false, errors.New("意外的授权请求：" + req.Relation + " " + req.Object)
	}
	if f.err != nil {
		return false, f.err
	}
	return !f.deny[req.Subject], nil
}

type fakeCoverage struct {
	covered func(domain, region string) bool
	err     error
	calls   []string
}

func (f *fakeCoverage) CheckCoverage(_ context.Context, domain, region string) (*commerce.CoverageDecision, error) {
	f.calls = append(f.calls, domain+"/"+region)
	if f.err != nil {
		return nil, f.err
	}
	return &commerce.CoverageDecision{Covered: f.covered(domain, region)}, nil
}

func viewer(id string) context.Context {
	return reqctx.With(context.Background(), reqctx.Context{Principal: reqctx.Principal{ID: id, Kind: "user"},
		Tenant: "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1", Cell: "cell-01"})
}

func sampleRun() *Run {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s := func(id, domain, region string) Sample {
		smp := Sample{FactID: id, FactType: "tendering.bid_result", OccurredAt: at, Draft: "review_candidate"}
		if domain != "" {
			smp.Scope = &SampleScope{InfoDomain: domain, Region: region}
		}
		return smp
	}
	return &Run{ID: "sim_x", Report: &Report{
		Samples: []Sample{s("F1", "INF-PROC", "330106"), s("F2", "INF-PROC", "440305"), s("F3", "INF-PROC", "330106"), s("F4", "", "")},
		Diff:    &Diff{AddedSamples: []Sample{s("F5", "INF-PROC", "440305")}, RemovedSamples: []Sample{}, ChangedSamples: []Sample{s("F6", "INF-INVP", "")}},
		Hints:   []Hint{{Code: "review_majority"}},
	}}
}

func factIDs(list []Sample) string {
	var out []string
	for _, s := range list {
		out = append(out, s.FactID)
	}
	return strings.Join(out, ",")
}

// D-17：代表样本按查看者本人的权限过滤；无权的只给数量；判断不可用时全部不返回并提示；保存的报告不变。
func TestPresentFiltersSamplesPerViewer(t *testing.T) {
	zhejiang := func(domain, region string) bool { return domain == "INF-PROC" && strings.HasPrefix(region, "33") }
	decider := &fakeDecider{deny: map[string]bool{"user:usr_outsider": true}}
	coverage := &fakeCoverage{covered: zhejiang}
	svc := &Service{Authz: decider, Coverage: coverage}
	stored := sampleRun()
	before, _ := json.Marshal(stored)

	got := svc.present(viewer("usr_applicant"), stored)
	r := got.Report
	if factIDs(r.Samples) != "F1,F3" || *r.SamplesWithheld != 2 || factIDs(r.Diff.AddedSamples) != "" || *r.Diff.AddedWithheld != 1 ||
		*r.Diff.RemovedWithheld != 0 || factIDs(r.Diff.ChangedSamples) != "" || *r.Diff.ChangedWithheld != 1 {
		t.Fatalf("%+v %+v", r, r.Diff)
	}
	// 同一（信息领域、区划）只问一次覆盖；没有对象范围的样本不问也不返回
	if len(coverage.calls) != 3 {
		t.Fatalf("%v", coverage.calls)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), `"scope"`) {
		t.Fatalf("响应不应带对象范围：%s", raw)
	}
	if after, _ := json.Marshal(stored); string(after) != string(before) {
		t.Fatalf("保存的报告被改动：%s", after)
	}
	// 同一查看者再次读取结果相同（确定性）
	again, _ := json.Marshal(svc.present(viewer("usr_applicant"), stored))
	if string(again) != string(raw) {
		t.Fatalf("%s\n%s", raw, again)
	}

	// 越权：没有 can_search_public 的查看者（如外部协作者）看不到任何样本，只看到数量；不问覆盖
	coverage.calls = nil
	r = svc.present(viewer("usr_outsider"), stored).Report
	if len(r.Samples) != 0 || *r.SamplesWithheld != 4 || *r.Diff.AddedWithheld != 1 || *r.Diff.ChangedWithheld != 1 || len(coverage.calls) != 0 {
		t.Fatalf("%+v %v", r, coverage.calls)
	}
	for _, h := range r.Hints {
		if h.Code == hintSamplesUnavailable {
			t.Fatal("无权不是不可用")
		}
	}

	// 失败关闭：授权或覆盖判断不可用时全部不返回，提示 samples_unavailable，不当作有权
	for name, s := range map[string]*Service{
		"授权不可用": {Authz: &fakeDecider{err: errors.New("down")}, Coverage: coverage},
		"覆盖不可用": {Authz: decider, Coverage: &fakeCoverage{err: errors.New("down")}},
		"未装配覆盖": {Authz: decider},
	} {
		r := s.present(viewer("usr_applicant"), stored).Report
		hinted := false
		for _, h := range r.Hints {
			hinted = hinted || h.Code == hintSamplesUnavailable
		}
		if len(r.Samples)+len(r.Diff.AddedSamples)+len(r.Diff.ChangedSamples) != 0 || *r.SamplesWithheld != 4 || *r.Diff.ChangedWithheld != 1 || !hinted {
			t.Fatalf("%s：%+v", name, r)
		}
	}
	if len(stored.Report.Hints) != 1 {
		t.Fatalf("保存的提示被改动：%+v", stored.Report.Hints)
	}
	// 覆盖判断中途失败：已判断为有权的样本同样不返回
	flaky := &fakeCoverage{covered: zhejiang}
	s := &Service{Authz: decider, Coverage: &failAfter{next: flaky, n: 1}}
	if r := s.present(viewer("usr_applicant"), stored).Report; len(r.Samples) != 0 || *r.SamplesWithheld != 4 {
		t.Fatalf("%+v", r)
	}

	// 越权：服务主体与缺少租户的上下文不返回样本
	svcCtx := reqctx.With(context.Background(), reqctx.Context{Principal: reqctx.Principal{ID: "svc:learning", Kind: "service"},
		Tenant: "tnt_01JA8ZXXXXXXXXXXXXXXXXXXX1", Cell: "cell-01"})
	if r := svc.present(svcCtx, stored).Report; len(r.Samples) != 0 || *r.SamplesWithheld != 4 {
		t.Fatalf("%+v", r)
	}
	if r := svc.present(context.Background(), stored).Report; len(r.Samples) != 0 {
		t.Fatalf("%+v", r)
	}
}

type failAfter struct {
	next commerce.Coverage
	n    int
}

func (f *failAfter) CheckCoverage(ctx context.Context, domain, region string) (*commerce.CoverageDecision, error) {
	if f.n == 0 {
		return nil, errors.New("down")
	}
	f.n--
	return f.next.CheckCoverage(ctx, domain, region)
}
