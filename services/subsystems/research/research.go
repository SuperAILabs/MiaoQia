// Package research 组装 research Agent 研究 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package research

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/research/api"
)

// Module 返回 research 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "res"}
}
