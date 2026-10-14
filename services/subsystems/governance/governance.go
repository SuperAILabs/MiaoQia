// Package governance 组装 governance 子系统（审计、数据分级、保留删除、合规登记）。M0 提供审计入库。
package governance

import (
	"github.com/superailabs/miaoqia/services/kernel/audit"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/subsystems/governance/api"
	"github.com/superailabs/miaoqia/services/subsystems/governance/internal/app"
	"github.com/superailabs/miaoqia/services/subsystems/governance/migrations"
)

// Module 返回 governance 的模块定义。
func Module() runtime.Module {
	return runtime.Module{
		Name:       api.Name,
		Database:   "gov",
		Migrations: migrations.FS,
		Consumers: func(env *runtime.Env) []*consumer.Consumer {
			return []*consumer.Consumer{{
				Name: "governance.audit-ingest", Subsystem: api.Name, Brokers: env.Brokers, DB: env.DB, Log: env.Log,
				Topics:   []string{audit.Topic},
				Handlers: map[string]consumer.Handler{audit.EventType: app.Ingest},
			}}
		},
	}
}
