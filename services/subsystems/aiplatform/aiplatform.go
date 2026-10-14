// Package aiplatform 组装 ai-platform AI 平台 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package aiplatform

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/aiplatform/api"
)

// Module 返回 ai-platform 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "aip"}
}
