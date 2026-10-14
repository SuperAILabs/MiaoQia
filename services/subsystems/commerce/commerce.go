// Package commerce 组装 commerce 订阅计量计费 子系统。接口、事件与数据随所属里程碑实现（docs/05-delivery/01-roadmap.md）。
package commerce

import (
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/commerce/api"
)

// Module 返回 commerce 的模块定义。
func Module() runtime.Module {
	return runtime.Module{Name: api.Name, Database: "com"}
}
