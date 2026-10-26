package pursuit

import (
	"context"
	"fmt"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/scheduler"
	identity "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit/api"
)

// jobBatch 是每轮每个租户每项处理的条数上限；未处理完的留到下一轮。
const jobBatch = 100

// jobs 是 pursuit 在 scheduler 角色中的周期任务（验收线 R-01）：经 identity.Tenants 枚举本 cell 的 active 租户，逐个租户以
// 服务主体 svc:pursuit 调用 Reminders（pursuit/api）。now 是业务时钟（与 Service 相同）。
func jobs(now func() time.Time) func(env *runtime.Env) ([]scheduler.Job, error) {
	return func(env *runtime.Env) ([]scheduler.Job, error) {
		tenants, ok := runtime.Lookup[identity.Tenants](env.Bindings, identity.TenantsBinding)
		if !ok {
			return nil, fmt.Errorf("缺少 %s 绑定", identity.TenantsBinding)
		}
		reminders, ok := runtime.Lookup[api.Reminders](env.Bindings, api.RemindersBinding)
		if !ok {
			return nil, fmt.Errorf("缺少 %s 绑定", api.RemindersBinding)
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
			{Name: "pursuit.run-due-reminders", Every: time.Minute,
				Run: perTenant(func(ctx context.Context) (int, error) { return reminders.RunDue(ctx, now(), jobBatch) })},
			{Name: "pursuit.retry-authorization-sync", Every: time.Minute,
				Run: perTenant(func(ctx context.Context) (int, error) { return reminders.RetrySync(ctx, jobBatch) })},
		}, nil
	}
}

func clock(now func() time.Time) func() time.Time {
	if now != nil {
		return now
	}
	return time.Now
}
