package app

import (
	"context"

	"github.com/superailabs/miaoqia/services/kernel/authz"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// 代表样本按查看者权限过滤（PRD 第7段 F3 第5条、用例11；验收线 D-17）。保存的报告包含全部代表样本及其对象范围；每次读取按当前
// 调用者判断每条样本对应的公共事实能否查看，与对象页的前置判断相同：租户上的 can_search_public（identity），以及对象所在信息领域
// 与区划在租户的数据覆盖包内（commerce）。无权的样本不返回，只计入同一列表的未返回数量；判断不可用时全部按未返回计数并提示
// samples_unavailable，不当作有权。没有对象范围的样本（信息领域未知）不返回。
const hintSamplesUnavailable = "samples_unavailable"

// sampleGate 是一次读取内的查看判断（同一信息领域与区划只问一次 commerce）。
type sampleGate struct {
	member      bool
	unavailable bool
	covered     func(domain, region string) (bool, error)
	cache       map[[2]string]bool
}

func (g *sampleGate) allow(s Sample) bool {
	if g.unavailable || !g.member || s.Scope == nil || s.Scope.InfoDomain == "" {
		return false
	}
	key := [2]string{s.Scope.InfoDomain, s.Scope.Region}
	if v, ok := g.cache[key]; ok {
		return v
	}
	ok, err := g.covered(s.Scope.InfoDomain, s.Scope.Region)
	if err != nil {
		g.unavailable = true
		return false
	}
	g.cache[key] = ok
	return ok
}

// gate 判断当前调用者是否为可查看公共事实的租户成员，并准备覆盖判断。调用者须为带租户的用户主体（其他情形全部不返回）。
func (s *Service) gate(ctx context.Context) *sampleGate {
	g := &sampleGate{cache: map[[2]string]bool{}}
	rc, ok := reqctx.From(ctx)
	if !ok || rc.Validate() != nil || rc.Tenant == "" || rc.Principal.Kind != "user" {
		return g
	}
	if s.Authz == nil || s.Coverage == nil {
		g.unavailable = true
		return g
	}
	member, err := s.Authz.Check(ctx, authz.Request{Subject: authz.Subject(rc.Principal), Relation: "can_search_public", Object: "tenant:" + rc.Tenant})
	if err != nil {
		g.unavailable = true
		return g
	}
	g.member = member
	g.covered = func(domain, region string) (bool, error) {
		d, err := s.Coverage.CheckCoverage(ctx, domain, region)
		if err != nil {
			return false, err
		}
		return d.Covered, nil
	}
	return g
}

// present 返回按当前调用者过滤代表样本后的运行副本（不改动 r）。全部列表都先判断完再决定是否不可用：判断中途不可用时，
// 已判断为有权的样本同样不返回，响应中每个列表的样本要么都经过判断、要么全部按未返回计数。
func (s *Service) present(ctx context.Context, r *Run) *Run {
	if r == nil || r.Report == nil {
		return r
	}
	out := *r
	report := *r.Report
	out.Report = &report
	g := s.gate(ctx)
	filter := func(list []Sample) ([]Sample, int) {
		kept := []Sample{}
		for _, smp := range list {
			if g.allow(smp) {
				smp.Scope = nil
				kept = append(kept, smp)
			}
		}
		return kept, len(list) - len(kept)
	}
	type slot struct {
		samples  *[]Sample
		withheld **int
		source   []Sample
	}
	var slots []slot
	if r.Report.Diff != nil {
		diff := *r.Report.Diff
		report.Diff = &diff
		slots = append(slots, slot{&diff.AddedSamples, &diff.AddedWithheld, r.Report.Diff.AddedSamples},
			slot{&diff.RemovedSamples, &diff.RemovedWithheld, r.Report.Diff.RemovedSamples},
			slot{&diff.ChangedSamples, &diff.ChangedWithheld, r.Report.Diff.ChangedSamples})
	}
	if r.Report.Samples != nil {
		slots = append(slots, slot{&report.Samples, &report.SamplesWithheld, r.Report.Samples})
	}
	for _, sl := range slots {
		kept, withheld := filter(sl.source)
		*sl.samples, *sl.withheld = kept, &withheld
	}
	if g.unavailable {
		for _, sl := range slots {
			n := len(sl.source)
			*sl.samples, *sl.withheld = []Sample{}, &n
		}
		report.Hints = append(append([]Hint{}, r.Report.Hints...), Hint{Code: hintSamplesUnavailable})
	}
	return &out
}
