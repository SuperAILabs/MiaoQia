// Package customercontext 组装 customer-context 客户上下文与策略 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package customercontext

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext/api"
)

// Module 返回 customer-context 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "ctx"}
}
