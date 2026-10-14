// Package assessment 组装 assessment 判断与决策 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package assessment

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/assessment/api"
)

// Module 返回 assessment 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "asm"}
}
