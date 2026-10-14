// Package pursuit 组装 pursuit 推进与结果 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package pursuit

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// Module 返回 pursuit 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "pur"}
}
