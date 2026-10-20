package api

import (
	"context"
	"errors"
	"time"
)

// 以下是匹配的出站端口（detection.md 第3节 candidate_generator 与 matcher 的视图组装）：
//
//   - Candidates 是订阅反查（search 的反查索引）：给定一条公共事实版本，返回本 cell 中可能命中的租户。只返回租户，进入租户处理前
//     detection 仍按该租户的生效策略与启用领域集合判断（detection.md 第9节）；
//   - Facts 是视图组装的读取（domain-intelligence 的事实视图投影与 entity-graph 的关联视图项）：给定一条公共事实版本，
//     返回其中的求值单位（如结果公告中的每条中标结果）及其视图字段、关联视图项、证据与展示字段。
//
// 生产适配器尚未接入：生产组装使用“未接入”实现（NotWired），调用返回 ErrNotWired，事件处理失败并按消费框架重试、转入死信，
// 不以空数据冒充“没有信号”。测试与演示使用标明为夹具的内存实现（api/facttest）。

// ErrNotWired 表示匹配适配器没有接入。
var ErrNotWired = errors.New("detection：匹配适配器未接入")

// FactRef 是一条公共事实版本（domain.fact.upserted.v1 的载荷与信封）。
type FactRef struct {
	ID            string
	Version       int64
	ObjectType    string
	InfoDomain    string
	ChangedFields []string
	EventID       string
	EventTime     time.Time
}

// Evidence 是视图字段的证据元数据（键为视图路径，如 fact.winner.org_id 或 fact.categories）。
type Evidence struct {
	Confidence float64
	Method     string // rule / model / manual
	Suspect    bool
}

// Subject 是求值单位的对象引用（契约 SubjectRef，type 为 domain_fact）：事实标识与版本，以及事实内的部分（如中标结果 bdr_）。
type Subject struct {
	ID      string
	Version int64
	PartID  string
}

// ItemRef 是视图快照中一项的对象与版本（ExplainSignal 的 inputs.view_items）。
type ItemRef struct {
	Name    string
	ID      string
	Version int64
	Domain  string
}

// Display 是收件箱行的展示字段（来自事实视图，不参与求值）。AmountValue 为空表示金额未公布，不以预算或其他金额代替。
type Display struct {
	Title          string
	WinnerOrgID    string
	WinnerName     string
	AmountValue    string // 十进制金额文本，如 18000000 或 1234.50
	AmountCurrency string
	AmountCNY      *int64 // 人民币元（整数），用于金额筛选与排序；非人民币或未公布为空
	Location       string
	Categories     []string
	LotKey         string // 同一标包的键（同标包多家中标时相同）
}

// Unit 是一个求值单位的视图（策略 DSL 第5.1节）。Key 在事实类型内唯一，是信号的去重键（同一目标对同一求值单位只有一条信号）。
// Fields 是事实视图字段（不含关联视图项）；Items 是关联视图项（如 winner.credit_flags），组装到 fact.<路径>；ItemDomains 是关联
// 视图项 → 提供它的信息领域（领域不在目标的启用领域集合中时该项不出现，读取为未知）。Domain 是提供事实类型的信息领域。
type Unit struct {
	Key         string
	FactType    string
	Domain      string
	Subject     Subject
	Event       string // 触发求值的领域事件类型（表达式中的 event.type）
	Fields      map[string]any
	Items       map[string]any
	ItemDomains map[string]string
	ItemRefs    []ItemRef
	Evidence    map[string]Evidence
	Display     Display
}

// Facts 读取一条公共事实版本中的求值单位。
type Facts interface {
	Units(ctx context.Context, ref FactRef) ([]Unit, error)
}

// Candidates 返回 cell 中可能命中该事实版本的租户。
type Candidates interface {
	Tenants(ctx context.Context, cell string, ref FactRef) ([]string, error)
}

// NotWired 是缺省的匹配适配器：所有调用返回 ErrNotWired。
type NotWired struct{}

func (NotWired) Units(context.Context, FactRef) ([]Unit, error)             { return nil, ErrNotWired }
func (NotWired) Tenants(context.Context, string, FactRef) ([]string, error) { return nil, ErrNotWired }
