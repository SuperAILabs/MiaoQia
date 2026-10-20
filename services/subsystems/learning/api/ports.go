package api

import (
	"context"
	"errors"
	"time"
)

// 以下是策略仿真的出站端口（PRD 第7段 F3；learning.md 第5节 SimulatePolicy）：
//
//   - Dataset 读取被回放的公共事实（M1 已启用来源）。生产适配器（公共事实的版本化快照）尚未接入；
//   - DomainStatus 读取信息领域服务状态（domain-intelligence GetCoverage）。生产适配器尚未接入。
//
// 生产组装使用“未接入”实现（NotWired）：调用返回 ErrNotWired，仿真运行失败（dataset_unavailable / domain_status_unavailable），
// 不以空数据冒充“没有信号”。测试与演示使用标明为夹具的内存实现（api/datasettest，报告 inputs.dataset_fixture 为 true）。

// ErrNotWired 表示仿真适配器没有接入。
var ErrNotWired = errors.New("learning：仿真适配器未接入")

// Evidence 是事实字段的证据元数据（键为视图路径，如 fact.winner.org_id）。
type Evidence struct {
	Confidence float64
	Method     string // rule / model / manual
	Suspect    bool
}

// FactVersion 是一条公共事实的一个版本。同一事实（ID）可有多个版本（更正）：回放时按逻辑求值时间（OccurredAt）只取当时已可得
// （KnownAt 不晚于 OccurredAt）的最新版本，之后才知道的更正不参与（时点正确，learning.md 8.1）。
type FactVersion struct {
	ID         string
	Type       string // 事实类型，如 tendering.bid_result
	Event      string // 触发求值的领域事件类型
	OccurredAt time.Time
	KnownAt    time.Time
	Title      string
	AmountCNY  *int64
	// Fields 是事实视图字段（不含关联视图项）；Items 是关联视图项（如 winner.credit_flags），组装到 fact.<路径>。
	Fields   map[string]any
	Items    map[string]any
	Evidence map[string]Evidence
}

// DatasetSlice 是一次读取的数据集切片。
type DatasetSlice struct {
	Version       string    // 数据集版本（固定输入的一部分）
	Fixture       bool      // 为 true 时是夹具数据（测试与演示），不是生产公共事实
	CoverageStart time.Time // 数据集可得历史的起点（窗口按实际可得截断）
	Facts         []FactVersion
	// FactDomains 是事实类型 → 提供它的信息领域；ViewDomains 是关联视图项 → 提供它的信息领域。
	FactDomains map[string]string
	ViewDomains map[string]string
}

// Dataset 读取 [from, to) 内发生的事实（含其全部版本）。
type Dataset interface {
	Load(ctx context.Context, from, to time.Time) (*DatasetSlice, error)
}

// 信息领域服务状态。
const (
	DomainLive    = "live"
	DomainNotLive = "not_live"
)

// DomainStatus 返回各信息领域的服务状态（live / not_live）；未返回的领域按 unknown 处理（不启用）。
type DomainStatus interface {
	Status(ctx context.Context, domains []string) (map[string]string, error)
}

// NotWired 是缺省的仿真适配器：所有调用返回 ErrNotWired。
type NotWired struct{}

func (NotWired) Load(context.Context, time.Time, time.Time) (*DatasetSlice, error) {
	return nil, ErrNotWired
}
func (NotWired) Status(context.Context, []string) (map[string]string, error) { return nil, ErrNotWired }
