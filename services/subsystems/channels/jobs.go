package channels

import (
	"context"
	"fmt"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/scheduler"
	"github.com/superailabs/miaoqia/services/subsystems/channels/api"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// jobBatch 是每轮每个租户每项处理的条数上限；未处理完的留到下一轮。
const jobBatch = 100

// jobs 是 channels 在 scheduler 角色中的周期任务（验收线 R-01）：经 identity.Tenants 枚举本 cell 的 active 租户，逐个租户以
// 服务主体 svc:channels 调用本子系统可重复执行的维护入口（channels/api：Maintenance、Briefings）。
// 简报组装（Compose）按成员进行，待 channels.Briefings 提供按到时批量组装的入口后再加入。
func jobs(env *runtime.Env) ([]scheduler.Job, error) {
	tenants, ok := runtime.Lookup[identity.Tenants](env.Bindings, identity.TenantsBinding)
	if !ok {
		return nil, fmt.Errorf("缺少 %s 绑定", identity.TenantsBinding)
	}
	maintenance, ok := runtime.Lookup[api.Maintenance](env.Bindings, api.MaintenanceBinding)
	if !ok {
		return nil, fmt.Errorf("缺少 %s 绑定", api.MaintenanceBinding)
	}
	briefings, ok := runtime.Lookup[api.Briefings](env.Bindings, api.BriefingsBinding)
	if !ok {
		return nil, fmt.Errorf("缺少 %s 绑定", api.BriefingsBinding)
	}
	cell := env.Cell
	perTenant := func(fn func(ctx context.Context) (int, error)) func(context.Context) (int, error) {
		return func(ctx context.Context) (int, error) {
			return identity.ForEachActiveTenant(ctx, tenants, cell, func(ctx context.Context, tenant string) (int, error) {
				return fn(reqctx.With(ctx, reqctx.Context{Tenant: tenant, Cell: cell, Principal: reqctx.Principal{ID: "svc:" + api.Name, Kind: "service"}}))
			})
		}
	}
	return []scheduler.Job{
		{Name: "channels.retry-revocations", Every: time.Minute,
			Run: perTenant(func(ctx context.Context) (int, error) { return maintenance.RetryRevocations(ctx, jobBatch) })},
		{Name: "channels.reconcile-sessions", Every: 15 * time.Minute, Timeout: 5 * time.Minute,
			Run: perTenant(maintenance.ReconcileChannelSessions)},
		{Name: "channels.deliver-briefings", Every: time.Minute,
			Run: perTenant(func(ctx context.Context) (int, error) { return briefings.DeliverDue(ctx, jobBatch) })},
		{Name: "channels.process-card-actions", Every: 15 * time.Second,
			Run: perTenant(func(ctx context.Context) (int, error) { return briefings.ProcessActions(ctx, jobBatch) })},
	}, nil
}
