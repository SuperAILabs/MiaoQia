// Package channels 组装 channels 渠道与会话 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package channels

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
)

// Module 返回 channels 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "chn"}
}
