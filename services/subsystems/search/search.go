// Package search 组装 search 检索与索引 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package search

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/search/api"
)

// Module 返回 search 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: ""}
}
