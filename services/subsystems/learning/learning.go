// Package learning 组装 learning 反馈学习与评估 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package learning

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/learning/api"
)

// Module 返回 learning 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "lrn"}
}
