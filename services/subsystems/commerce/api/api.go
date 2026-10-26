// Package api 是 commerce 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/commerce.md。
package api

import (
	"context"
	"time"

	commerceapi "github.com/superailabs/miaoqia/services/contracts/apis/commerce"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// Name 是子系统名。
const Name = "commerce"

// 查看计量的种类（契约 ViewKind）。
const (
	ObjectView   = "object_view"
	EvidenceView = "evidence_view"
)

// MeteringBinding 是查看计量接口在绑定表中的名称。
const MeteringBinding = "commerce.ViewMetering"

// Receipt 是契约生成类型（contracts/apis/commerce/openapi.yaml 的 ViewMeterReceipt）。
type Receipt = commerceapi.ViewMeterReceipt

// ViewMetering 是对象查看与原文查看的计量（PRD 第4段 F9；gateway 以请求上下文中的用户主体调用）。计量操作标识为（租户、查看者、
// 种类、对象或证据、计量日），同一计量操作只计一次（之后为 unchanged）；requestKey 是本次页面请求（尝试）的键，同键重发返回该尝试
// 的结果（含 quota.exhausted）。额度已用完时返回 429 quota.exhausted 的 *Error，不写入计量操作。不可用时返回 503 或暂时性错误，
// 调用方按 PRD 第4段第13节第3项放行并事后补记。
type ViewMetering interface {
	RecordView(ctx context.Context, kind, target, requestKey string) (*Receipt, error)
}

// LocalMetering 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func LocalMetering(impl ViewMetering) ViewMetering { return localMetering{impl} }

type localMetering struct{ impl ViewMetering }

func (l localMetering) RecordView(ctx context.Context, kind, target, requestKey string) (*Receipt, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RecordView", Idempotent: true, Timeout: 3 * time.Second},
		func(ctx context.Context) (*Receipt, error) { return l.impl.RecordView(ctx, kind, target, requestKey) })
}

// Error 是可在传输边界暴露的错误（稳定 code，commerce 第8节）。quota.exhausted 时带已用量、上限与重置时间。
type Error struct {
	Status   int
	Code     string
	Detail   string
	Used     int64
	Limit    int64
	ResetsAt time.Time
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Code + "：" + e.Detail
	}
	return e.Code
}

// CoverageBinding 是覆盖判断接口在绑定表中的名称。
const CoverageBinding = "commerce.Coverage"

// 覆盖判断缺少的维度（契约 CoverageDecision.missing）。
const (
	MissingInfoDomain = "info_domain"
	MissingRegion     = "region"
)

// CoverageDecision 是一次覆盖判断：Covered 为假时 Missing 列出缺少的覆盖维度。
type CoverageDecision struct {
	Covered bool
	Missing []string
}

// Coverage 是 GetEntitlements 的数据覆盖部分（commerce 第4、8节）：对象所在信息领域与行政区划（GB/T 2260 六位代码，未知时为空）
// 是否在请求上下文中租户的数据覆盖包内。调用者为用户主体。覆盖包不可用时返回 503 coverage_unavailable 的 *Error 或暂时性错误，
// 调用方失败关闭，不当作覆盖内或覆盖外。
type Coverage interface {
	CheckCoverage(ctx context.Context, infoDomain, region string) (*CoverageDecision, error)
}

// LocalCoverage 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func LocalCoverage(impl Coverage) Coverage { return localCoverage{impl} }

type localCoverage struct{ impl Coverage }

func (l localCoverage) CheckCoverage(ctx context.Context, infoDomain, region string) (*CoverageDecision, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "CheckCoverage", Idempotent: true, Timeout: 2 * time.Second},
		func(ctx context.Context) (*CoverageDecision, error) {
			return l.impl.CheckCoverage(ctx, infoDomain, region)
		})
}
