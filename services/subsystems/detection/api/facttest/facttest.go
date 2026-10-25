// Package facttest 是匹配端口（detection api.Facts 与 api.Candidates）的内存实现，只用于测试与演示。
//
// 这里的事实全部是夹具数据（FIXTURE）：不是公共事实，不来自任何真实来源，标题、金额、主体与地区均为虚构，标识只借用登记前缀的
// 格式。不做任何外部调用。生产适配器（domain-intelligence 的事实视图投影、entity-graph 的关联视图项与 search 的订阅反查）尚未接入。
package facttest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// ErrInjected 是测试注入的故障（视图组装或订阅反查不可用）。
var ErrInjected = errors.New("facttest：注入的故障")

// ItemDomains 是夹具的关联视图项到信息领域的对应（与 learning 的 datasettest 一致）。
var ItemDomains = map[string]string{
	"winner.profile":               "INF-ORG",
	"winner.credit_flags":          "INF-CRED",
	"winner.history_categories":    "INF-PROC",
	"winner.supplied_brand_owners": "INF-PROC",
}

// Store 是内存的事实视图与订阅反查。
type Store struct {
	mu       sync.Mutex
	units    map[string][]api.Unit
	links    map[string][]string
	tenants  []string
	fail     bool
	requests int
}

// New 返回空的夹具；tenants 是订阅反查返回的租户（夹具不按事实筛选租户，进入租户处理后仍按生效策略判断）。
func New(tenants ...string) *Store {
	return &Store{units: map[string][]api.Unit{}, links: map[string][]string{}, tenants: tenants}
}

// Link 登记项目 prj_ 的归属采购项目（夹具的 part_of 边，全部视为已确认）。
func (s *Store) Link(project string, procurementProjects ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links[project] = append([]string(nil), procurementProjects...)
}

// ProcurementProjects 实现 api.ProjectLinks。
func (s *Store) ProcurementProjects(_ context.Context, project string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, ErrInjected
	}
	return append([]string(nil), s.links[project]...), nil
}

func key(id string, version int64) string { return id + "@" + strconv.FormatInt(version, 10) }

// Put 登记一条事实版本中的求值单位。
func (s *Store) Put(factID string, version int64, units ...api.Unit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.units[key(factID, version)] = append(s.units[key(factID, version)], units...)
}

// SetTenants 改变订阅反查返回的租户。
func (s *Store) SetTenants(tenants ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants = tenants
}

// Fail 注入视图组装与订阅反查的故障。
func (s *Store) Fail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

// Requests 返回视图组装被调用的次数。
func (s *Store) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Units 实现 api.Facts：未登记的事实版本没有求值单位。
func (s *Store) Units(_ context.Context, ref api.FactRef) ([]api.Unit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if s.fail {
		return nil, ErrInjected
	}
	return append([]api.Unit(nil), s.units[key(ref.ID, ref.Version)]...), nil
}

// Tenants 实现 api.Candidates。
func (s *Store) Tenants(context.Context, string, api.FactRef) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, ErrInjected
	}
	return append([]string(nil), s.tenants...), nil
}

// Bid 描述一条夹具中标结果（tendering.bid_result 的求值单位：结果公告 ntc_ 中的一条结果 bdr_）。
type Bid struct {
	Notice, Result, Lot string // ntc_、bdr_ 与标包键
	Project             string // 所在的采购项目 pp_（可为空）
	Buyer, BuyerName    string // 采购人 org_（可为空）
	Version             int64  // 公告版本，缺省 1
	Title               string
	Winner, WinnerName  string // org_；Winner 为空时视图中没有中标方
	Amount              int64  // 人民币元；0 表示未公布（视图中不出现，不以预算代替）
	Location            string
	Categories          []string // 品类代码；nil 表示视图中没有品类
	CategoryConfidence  float64  // 品类增强的证据置信度；0 表示不带证据元数据
	WinnerConfidence    float64  // 中标方识别的证据置信度；0 表示不带证据元数据
	Candidate           bool     // 候选人公示（W-CANDIDATE）
	HistoryCategories   []string // 中标方近三年中标品类（winner.history_categories）
	CreditFlags         []any    // 中标方信用记录（winner.credit_flags，INF-CRED）
}

// BidResult 返回中标结果的求值单位（A02 结果视图，packs/application_domains/tendering/policy/views.yaml）。
func BidResult(b Bid) api.Unit {
	if b.Version == 0 {
		b.Version = 1
	}
	fields := map[string]any{"result_type": "awarded", "location": b.Location}
	if b.Candidate {
		fields["result_type"] = "candidate_published"
	}
	display := api.Display{Title: b.Title, WinnerOrgID: b.Winner, WinnerName: b.WinnerName, Location: b.Location, LotKey: b.Lot,
		Categories: append([]string{}, b.Categories...), ProcurementProjectID: b.Project, BuyerOrgID: b.Buyer, BuyerName: b.BuyerName}
	if b.Categories != nil {
		cats := []any{}
		for _, c := range b.Categories {
			cats = append(cats, map[string]any{"scheme": "industry:building-materials", "code": c})
		}
		fields["categories"] = cats
	}
	if b.Amount > 0 {
		v := b.Amount
		fields["amount"] = map[string]any{"value": fmt.Sprint(v), "currency": "CNY"}
		display.AmountValue, display.AmountCurrency, display.AmountCNY = fmt.Sprint(v), "CNY", &v
	}
	if b.Winner != "" {
		fields["winner"] = map[string]any{"org_id": b.Winner}
	}
	history := []any{}
	for _, c := range b.HistoryCategories {
		history = append(history, c)
	}
	credit := b.CreditFlags
	if credit == nil {
		credit = []any{}
	}
	items := map[string]any{"winner.credit_flags": credit, "winner.history_categories": history, "winner.supplied_brand_owners": []any{}}
	evidence := map[string]api.Evidence{}
	if b.CategoryConfidence > 0 {
		evidence["fact.categories"] = api.Evidence{Confidence: b.CategoryConfidence, Method: "model"}
	}
	if b.WinnerConfidence > 0 {
		evidence["fact.winner.org_id"] = api.Evidence{Confidence: b.WinnerConfidence, Method: "model"}
	}
	refs := []api.ItemRef{{Name: "fact", ID: b.Notice, Version: b.Version, Domain: "INF-PROC"}}
	if b.Winner != "" {
		refs = append(refs, api.ItemRef{Name: "winner.profile", ID: b.Winner, Version: 1, Domain: "INF-ORG"})
	}
	return api.Unit{Key: "tendering.bid_result:" + b.Notice + ":" + b.Result, FactType: "tendering.bid_result", Domain: "INF-PROC",
		Subject: api.Subject{ID: b.Notice, Version: b.Version, PartID: b.Result}, Event: "tendering.result.published.v1",
		Fields: fields, Items: items, ItemDomains: ItemDomains, ItemRefs: refs, Evidence: evidence, Display: display}
}
