// Package detection 组装 detection 监测与匹配 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package detection

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/detection/api"
)

// Module 返回 detection 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "det"}
}
