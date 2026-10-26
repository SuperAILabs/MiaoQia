// Package api 是 gateway 的公开接口。规格见 docs/03-architecture/subsystems/gateway.md。
package api

import (
	"context"
	"errors"
	"time"
)

// Name 是子系统名。
const Name = "gateway"

// 对象页 BFF 的出站端口（PRD 第4段第8节；gateway.md 第10节）。BFF 不实现业务规则，只按调用方现场裁剪并组装分区：
//
//   - PublicData 是数据平面的对象读取：对象解析（entity-graph GetEntity：旧标识经合并历史解析，按口径与记录截止时点）与分区内容
//     （domain-intelligence GetFact / GetFactHistory / QueryFacts / GetProjectChain / GetDomainStatistics，entity-graph
//     GetEntityProfile / GetRelationships）。分区请求带口径（Basis），响应须回带同一口径，BFF 据此核对，不一致时不展示；
//   - EvidenceResolver 是 evidence 的 ResolveEvidence：按调用方（请求上下文中的租户与用途）当前的来源权利返回展示级别与该级别允许的内容；
//   - Entitlements 是 commerce 权益中的数据覆盖判断（区域、信息领域）。
//
// 生产适配器尚未接入（数据平面的读取接口与权益随 INF-ORG、M2-P8、M2-P11 交付，见 docs/03-architecture/13-contract-readiness.md 第9节）：
// 生产组装使用“未接入”实现（NotWired），调用返回 ErrNotWired，对象页与证据面板 503，分区为 unavailable，不以空数据冒充“没有记录”。
// 测试与演示使用标明为夹具的内存实现（api/publictest）。

// ErrNotWired 表示数据平面或权益适配器没有接入。
var ErrNotWired = errors.New("gateway：对象页适配器未接入")

// ErrNotFound 表示对象或证据不存在（按所用口径）。
var ErrNotFound = errors.New("gateway：对象或证据不存在")

// 视角（契约 View）。
const (
	ViewCurrent    = "current"
	ViewSignalTime = "signal_time"
)

// Pinned 是信号视图快照中固定的一项（对象与版本）。
type Pinned struct {
	Name    string
	ID      string
	Version int64
}

// Basis 是所用口径：current 为业务有效口径；signal_time 为当时已知口径，RecordedCutoff 为记录截止时点（信号的 evaluated_at），
// Pinned 为视图快照中固定的版本（按固定版本直接读取，不经截止时点重新解析）。
type Basis struct {
	View           string
	RecordedCutoff time.Time
	Pinned         []Pinned
}

// Same 判断两个口径是否相同（视角与记录截止时点）。
func (b Basis) Same(o Basis) bool {
	return b.View == o.View && b.RecordedCutoff.Equal(o.RecordedCutoff)
}

// Merge 是一次合并或撤销（state 为 merged 或 reverted）。
type Merge struct {
	State string
	OrgID string
	At    time.Time
	Basis string
}

// Object 是解析后的对象。Type 为 procurement_project、project 或 organization；Region（行政区划代码）与 InfoDomain 用于覆盖判断。
type Object struct {
	ID           string
	RequestedID  string
	Type         string
	Name         string
	Region       string
	InfoDomain   string
	MergedFrom   []Merge
	CurrentMerge *Merge
}

// SectionRequest 是一个分区的读取。Categories 为“含我方品类”筛选（本租户档案品类，只用于本次查询，不写入公共数据）。
type SectionRequest struct {
	Object     Object
	Section    string
	Basis      Basis
	Cursor     string
	Limit      int
	Categories []string
}

// SectionData 是分区内容。Status 为 ok、empty、unavailable、history_unavailable 或 not_connected；Basis 为实际所用口径。
type SectionData struct {
	Status        string
	ReasonCode    string
	Basis         Basis
	AsOf          time.Time
	TimeAxis      string
	CoverageStart time.Time
	StatsVersion  string
	Items         []map[string]any
	NextCursor    string
	LaterCount    *int
}

// PublicData 是数据平面的对象读取。
type PublicData interface {
	Resolve(ctx context.Context, id string, basis Basis) (*Object, error)
	Section(ctx context.Context, req SectionRequest) (*SectionData, error)
}

// 展示级别（契约 DisplayLevel），从宽到严。
const (
	FullText     = "full_text"
	Excerpt      = "excerpt"
	CitationOnly = "citation_only"
	NoDisplay    = "none"
)

// EvidenceSource 是证据的来源信息。
type EvidenceSource struct {
	Name, Title, URL string
	Reachable        bool
	UnreachableSince time.Time
}

// EvidenceInput 是派生证据的一项输入。
type EvidenceInput struct {
	ID           string
	DisplayLevel string
}

// Evidence 是解析后的证据（evidence 已按展示级别裁剪）。ObjectID 是证据所属的对象（覆盖判断）。
type Evidence struct {
	ID              string
	Kind            string // document / record / derivation
	DisplayLevel    string
	ReasonCode      string
	ObjectID        string
	Source          EvidenceSource
	SnapshotSavedAt time.Time
	MetadataOnly    bool
	Retracted       bool
	Snippet         string
	Context         string
	Locator         string
	RecordKey       string
	RecordValue     string
	Transform       string
	Inputs          []EvidenceInput
}

// EvidenceResolver 解析证据（purpose 为调用用途，例如 object_page）。
type EvidenceResolver interface {
	Resolve(ctx context.Context, id, purpose string) (*Evidence, error)
}

// Entitlements 判断对象是否在请求上下文中租户的数据覆盖范围内。
type Entitlements interface {
	Covered(ctx context.Context, object Object) (bool, error)
}

// NotWired 是缺省的适配器：所有调用返回 ErrNotWired。
type NotWired struct{}

func (NotWired) Resolve(context.Context, string, Basis) (*Object, error) { return nil, ErrNotWired }
func (NotWired) Section(context.Context, SectionRequest) (*SectionData, error) {
	return nil, ErrNotWired
}
func (NotWired) Covered(context.Context, Object) (bool, error) { return false, ErrNotWired }

// NotWiredEvidence 是缺省的证据适配器（返回 ErrNotWired）。
type NotWiredEvidence struct{}

func (NotWiredEvidence) Resolve(context.Context, string, string) (*Evidence, error) {
	return nil, ErrNotWired
}
