// Package api 是 customer-context 的公开接口（其他子系统唯一可导入的部分）。规格见 docs/03-architecture/subsystems/customer-context.md。
package api

import (
	"context"

	customercontextapi "github.com/superailabs/miaoqia/services/contracts/apis/customer-context"
	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// Name 是子系统名。
const Name = "customer-context"

// ContextBinding 是上下文读取接口在绑定表中的名称。
const ContextBinding = "customer-context.ContextReader"

// RootUnit 是租户根组织单元。identity 登记 OrgUnit 标识前，快照与目标只支持根单元（契约 info.description）。
const RootUnit = "root"

// Snapshot 与 Version 是契约生成类型（contracts/apis/customer-context/openapi.yaml）。
type (
	Snapshot = customercontextapi.ContextSnapshot
	Version  = customercontextapi.ContextVersion
)

// ContextReader 是客户上下文的同步读取接口（detection、learning、research、assessment 使用）。只读、幂等。
//   - ContextVersion 读主库返回租户当前上下文版本；调用方每批求值前读取，缓存较旧时重新加载快照；
//   - ContextSnapshot 按（租户、组织单元、上下文版本）返回不可变快照，version 为 0 时取当前版本。
type ContextReader interface {
	ContextVersion(ctx context.Context) (*Version, error)
	ContextSnapshot(ctx context.Context, orgUnit string, version int64) (*Snapshot, error)
}

// Local 把本地实现包装为经调用管线的接口（与远程绑定行为一致）。
func Local(impl ContextReader) ContextReader { return local{impl} }

type local struct{ impl ContextReader }

func (l local) ContextVersion(ctx context.Context) (*Version, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetContextVersion", Idempotent: true}, l.impl.ContextVersion)
}

func (l local) ContextSnapshot(ctx context.Context, orgUnit string, version int64) (*Snapshot, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetContextSnapshot", Idempotent: true},
		func(ctx context.Context) (*Snapshot, error) { return l.impl.ContextSnapshot(ctx, orgUnit, version) })
}

// PolicyBinding 是策略仿真输入读取接口在绑定表中的名称。
const PolicyBinding = "customer-context.PolicyReader"

// SimulationInput 是契约生成类型（PolicySimulationInput）。
type SimulationInput = customercontextapi.PolicySimulationInput

// PolicyReader 是策略草案的仿真输入读取接口（learning 使用；有 tenant#can_read_context 的服务主体）。只读、幂等：
// 返回草案修订的合成文档与校验结果，以及读取时的生效版本（对比一侧）与策略代次。
type PolicyReader interface {
	SimulationInput(ctx context.Context, objective string, revision int64) (*SimulationInput, error)
}

// LocalPolicy 把本地实现包装为经调用管线的接口。
func LocalPolicy(impl PolicyReader) PolicyReader { return localPolicy{impl} }

type localPolicy struct{ impl PolicyReader }

func (l localPolicy) SimulationInput(ctx context.Context, objective string, revision int64) (*SimulationInput, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetPolicySimulationInput", Idempotent: true},
		func(ctx context.Context) (*SimulationInput, error) {
			return l.impl.SimulationInput(ctx, objective, revision)
		})
}

// AccessBinding 是目标访问判断接口在绑定表中的名称。
const AccessBinding = "customer-context.ObjectiveAccess"

// ObjectiveAccess 以请求上下文中的用户主体判断其对目标的关系（本子系统的口径：移除参与人在本地提交后立即撤权，
// 不等待 identity 同步）。返回被允许的关系（总含 can_view）；不能查看与不存在同样返回 404 objective_not_found 的 *Error；
// 授权服务不可用为 503。只读、幂等。
type ObjectiveAccess interface {
	Access(ctx context.Context, objective string, relations []string) ([]string, error)
}

// LocalAccess 把本地实现包装为经调用管线的接口。
func LocalAccess(impl ObjectiveAccess) ObjectiveAccess { return localAccess{impl} }

type localAccess struct{ impl ObjectiveAccess }

func (l localAccess) Access(ctx context.Context, objective string, relations []string) ([]string, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "GetObjectiveAccess", Idempotent: true},
		func(ctx context.Context) ([]string, error) {
			return l.impl.Access(ctx, objective, relations)
		})
}

// Error 是可在传输边界暴露的错误（稳定 code）；基础设施错误不直接暴露。
type Error struct {
	Status int
	Code   string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Code + "：" + e.Detail
	}
	return e.Code
}
