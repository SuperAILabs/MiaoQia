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

// composeBatch 是每次 ComposeDue 组装的成员数上限；More 为真时在本轮内继续，直到本轮超时。
const composeBatch = 50

// jobs 是 channels 在 scheduler 角色中的周期任务（验收线 R-01）：经 identity.Tenants 枚举本 cell 的 active 租户，逐个租户以
// 服务主体 svc:channels 调用本子系统可重复执行的维护入口（channels/api：Maintenance、Briefings、Notices）。now 是业务时钟（与 Service 相同）。
func jobs(now func() time.Time) func(env *runtime.Env) ([]scheduler.Job, error) {
	return func(env *runtime.Env) ([]scheduler.Job, error) { return declare(env, now) }
}

func declare(env *runtime.Env, now func() time.Time) ([]scheduler.Job, error) {
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
	notices, ok := runtime.Lookup[api.Notices](env.Bindings, api.NoticesBinding)
	if !ok {
		return nil, fmt.Errorf("缺少 %s 绑定", api.NoticesBinding)
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
		{Name: "channels.compose-briefings", Every: time.Minute,
			Run: perTenant(func(ctx context.Context) (int, error) { return composeDue(ctx, briefings, now()) })},
		{Name: "channels.deliver-briefings", Every: time.Minute,
			Run: perTenant(func(ctx context.Context) (int, error) { return briefings.DeliverDue(ctx, jobBatch) })},
		{Name: "channels.process-card-actions", Every: 15 * time.Second,
			Run: perTenant(func(ctx context.Context) (int, error) { return briefings.ProcessActions(ctx, jobBatch) })},
		{Name: "channels.deliver-notices", Every: time.Minute,
			Run: perTenant(func(ctx context.Context) (int, error) { return notices.DeliverNotices(ctx, jobBatch) })},
	}, nil
}

// composeDue 为一个租户组装 at 时到期的简报：More 为真时继续调用；一批没有任何成员得到结果（全部失败）时停止，留到下一轮重试，
// 不在本轮内反复重试同一批。返回新组装的数量；有成员组装失败时返回错误（本轮记为失败）。
func composeDue(ctx context.Context, briefings api.Briefings, at time.Time) (int, error) {
	composed, failed := 0, 0
	for {
		r, err := briefings.ComposeDue(ctx, at, composeBatch)
		if err != nil {
			return composed, err
		}
		composed += r.Composed
		failed += r.Failed
		if !r.More || r.Composed+r.Existing+r.Late+r.Skipped == 0 {
			break
		}
	}
	if failed > 0 {
		return composed, fmt.Errorf("%d 名成员的简报组装失败", failed)
	}
	return composed, nil
}

func clock(now func() time.Time) func() time.Time {
	if now != nil {
		return now
	}
	return time.Now
}
