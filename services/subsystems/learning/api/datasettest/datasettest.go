// Package datasettest 是策略仿真端口（learning api.Dataset 与 api.DomainStatus）的内存实现，只用于测试与演示。
//
// 这里的事实全部是夹具数据（FIXTURE）：不是公共事实，不来自任何真实来源，金额、主体与地区均为虚构；读取结果标为
// Fixture，仿真报告的 inputs.dataset_fixture 为 true。不做任何外部调用。
package datasettest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/superailabs/miaoqia/services/subsystems/learning/api"
)

// ErrInjected 是测试注入的故障（数据集或领域状态不可用）。
var ErrInjected = errors.New("datasettest：注入的故障")

// FactDomains 与 ViewDomains 是夹具的事实类型与关联视图项到信息领域的对应（生产对应随 domain-intelligence 的覆盖接口接入）。
var (
	FactDomains = map[string]string{
		"tendering.bid_result":           "INF-PROC",
		"tendering.contract_record":      "INF-PROC",
		"investment.construction_permit": "INF-INVP",
	}
	ViewDomains = map[string]string{
		"winner.profile":               "INF-ORG",
		"winner.credit_flags":          "INF-CRED",
		"winner.history_categories":    "INF-PROC",
		"winner.supplied_brand_owners": "INF-PROC",
	}
)

// Fixture 是内存数据集与领域状态。每次修改数据集都会改变数据集版本（仿真输入键随之变化）。
type Fixture struct {
	mu         sync.Mutex
	facts      []api.FactVersion
	coverage   time.Time
	revision   int
	status     map[string]string
	failLoad   bool
	failStatus bool
	loads      int
}

// New 返回空的夹具：可得历史从 coverage 起，全部信息领域为 live（可用 SetStatus 改变）。
func New(coverage time.Time) *Fixture {
	return &Fixture{coverage: coverage.UTC(), revision: 1, status: map[string]string{
		"INF-PROC": api.DomainLive, "INF-INVP": api.DomainLive, "INF-ORG": api.DomainLive, "INF-CRED": api.DomainLive}}
}

// Add 追加事实版本。
func (f *Fixture) Add(v ...api.FactVersion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = append(f.facts, v...)
	f.revision++
}

// SetCoverage 改变可得历史的起点。
func (f *Fixture) SetCoverage(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.coverage, f.revision = t.UTC(), f.revision+1
}

// SetStatus 设置信息领域的服务状态；status 为空时删除（读取结果中不出现，按 unknown 处理）。
func (f *Fixture) SetStatus(domain, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == "" {
		delete(f.status, domain)
		return
	}
	f.status[domain] = status
}

// FailLoad 与 FailStatus 注入数据集与领域状态的故障。
func (f *Fixture) FailLoad(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failLoad = fail
}

func (f *Fixture) FailStatus(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = fail
}

// Loads 返回数据集被读取的次数（核对相同输入复用运行、不重复计算）。
func (f *Fixture) Loads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

// Load 实现 api.Dataset。
func (f *Fixture) Load(_ context.Context, from, to time.Time) (*api.DatasetSlice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.failLoad {
		return nil, ErrInjected
	}
	out := &api.DatasetSlice{Version: fmt.Sprintf("fixture-%d", f.revision), Fixture: true, CoverageStart: f.coverage,
		FactDomains: FactDomains, ViewDomains: ViewDomains}
	for _, v := range f.facts {
		if !v.OccurredAt.Before(from) && v.OccurredAt.Before(to) && !v.OccurredAt.Before(f.coverage) {
			out.Facts = append(out.Facts, v)
		}
	}
	return out, nil
}

// Status 实现 api.DomainStatus。
func (f *Fixture) Status(_ context.Context, domains []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStatus {
		return nil, ErrInjected
	}
	out := map[string]string{}
	for _, d := range domains {
		if s, ok := f.status[d]; ok {
			out[d] = s
		}
	}
	return out, nil
}

func cny(v int64) map[string]any { return map[string]any{"value": fmt.Sprint(v), "currency": "CNY"} }

func pipe() []any {
	return []any{map[string]any{"scheme": "industry:building-materials", "code": "CAT-PIPE-PE"}}
}

func award(id string, at time.Time, title, location string, amount int64, winner string) api.FactVersion {
	a := amount
	return api.FactVersion{ID: id, Type: "tendering.bid_result", Event: "tendering.result.published.v1", OccurredAt: at, KnownAt: at, Title: title,
		AmountCNY: &a, Fields: map[string]any{"result_type": "awarded", "location": location, "categories": pipe(), "amount": cny(amount),
			"winner": map[string]any{"org_id": winner}},
		Items: map[string]any{"winner.credit_flags": []any{}, "winner.history_categories": []any{}, "winner.supplied_brand_owners": []any{}}}
}

// A02 返回一组覆盖 A02 进入供货模板主要分支的夹具事实（均为虚构），事件时间在 end 之前 1–120 天；self 是租户我方主体
// （档案 org_ids 中的主体）。预期（按模板缺省、服务区域 330000、品类 CAT-PIPE-PE、全部领域 live）：
//
//	FX-A02-01 大额、中标方近三年买过我方品类                 → 跟进候选
//	FX-A02-02 小额、无加分                                   → 复核候选
//	FX-A02-03 候选人公示                                     → 观察
//	FX-A02-04 大额、中标方有未了结的强制执行（信用领域）       → 复核候选（INF-CRED 未启用时规则未知，按 60 分为跟进候选）
//	FX-A02-05 服务区域外                                     → 无信号
//	FX-A02-06 中标方是我方主体                               → 排除
//	FX-A02-07 施工许可（投资项目领域）                        → 复核候选（INF-INVP 未启用时不回放）
//	FX-A02-08 首版 200 万，事件后才更正为 2000 万              → 复核候选（更正晚于逻辑求值时间，不参与）
//	FX-A02-09 缺品类字段                                     → 复核候选（R-CAT 未知：field_missing）
//	FX-A02-10 中标方识别置信度 0.5                           → 复核候选（R-WINNER 未知：below_evidence_threshold）
//	FX-A02-11 事件在 end 之前 120 天（默认窗口外）             → 不回放
func A02(end time.Time, self string) []api.FactVersion {
	day := func(n int) time.Time { return end.UTC().AddDate(0, 0, -n) }
	f1 := award("FX-A02-01", day(3), "夹具：某市政管网改造中标结果", "330106", 18_000_000, "org_fx_p1")
	f1.Items["winner.history_categories"] = []any{"CAT-PIPE-PE"}
	f2 := award("FX-A02-02", day(5), "夹具：某小区给水管更换中标结果", "330106", 1_000_000, "org_fx_p2")
	f3 := award("FX-A02-03", day(8), "夹具：某园区排水工程候选人公示", "330106", 0, "org_fx_p3")
	f3.Fields["result_type"], f3.AmountCNY = "candidate_published", nil
	delete(f3.Fields, "amount")
	f4 := award("FX-A02-04", day(10), "夹具：某道路排水工程中标结果", "330106", 9_000_000, "org_fx_p4")
	f4.Items["winner.credit_flags"] = []any{map[string]any{"record_type": "enforcement", "resolved": false}}
	f5 := award("FX-A02-05", day(12), "夹具：区外某管廊工程中标结果", "440305", 30_000_000, "org_fx_p5")
	f6 := award("FX-A02-06", day(15), "夹具：我方主体中标的管网工程", "330106", 6_000_000, self)
	permit := day(20)
	f7 := api.FactVersion{ID: "FX-A02-07", Type: "investment.construction_permit", Event: "investment.construction_permit.issued.v1", OccurredAt: permit,
		KnownAt: permit, Title: "夹具：某厂房施工许可", Fields: map[string]any{"location": "330106", "categories": pipe(), "investment_project_id": "FX-INV-07"}}
	f8 := award("FX-A02-08", day(25), "夹具：某泵站配套管网中标结果", "330106", 2_000_000, "org_fx_p8")
	f8b := award("FX-A02-08", day(25), "夹具：某泵站配套管网中标结果（更正）", "330106", 20_000_000, "org_fx_p8")
	f8b.KnownAt = day(2)
	f9 := award("FX-A02-09", day(30), "夹具：某片区雨污分流中标结果", "330106", 1_500_000, "org_fx_p9")
	delete(f9.Fields, "categories")
	f10 := award("FX-A02-10", day(40), "夹具：某学校管网中标结果", "330106", 1_200_000, "org_fx_p10")
	f10.Evidence = map[string]api.Evidence{"fact.winner.org_id": {Confidence: 0.5, Method: "model"}}
	f11 := award("FX-A02-11", day(120), "夹具：窗口外的中标结果", "330106", 18_000_000, "org_fx_p11")
	out := []api.FactVersion{f1, f2, f3, f4, f5, f6, f7, f8, f8b, f9, f10, f11}
	slices.SortStableFunc(out, func(a, b api.FactVersion) int { return a.OccurredAt.Compare(b.OccurredAt) })
	return out
}
