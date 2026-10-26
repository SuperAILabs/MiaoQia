package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/scheduler"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// maintenanceBatch 是每轮每项维护处理的条数上限；未处理完的留到下一轮。
const maintenanceBatch = 100

// jobs 是 identity 在 scheduler 角色中的周期任务（验收线 R-01）：经 Maintenance 本地绑定以服务主体 svc:identity 调用，
// 三项维护均可重复执行（identity/api/guard.go）。审批到期的业务时限为 7 天，按分钟检查即可满足 PRD 第7段用例13。
func jobs(env *runtime.Env) ([]scheduler.Job, error) {
	maintenance, ok := runtime.Lookup[api.Maintenance](env.Bindings, api.MaintenanceBinding)
	if !ok {
		return nil, fmt.Errorf("缺少 %s 绑定", api.MaintenanceBinding) // Start 已登记，缺失是装配错误
	}
	svc := func(ctx context.Context) context.Context {
		return reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: "svc:" + api.Name, Kind: "service"}})
	}
	return []scheduler.Job{
		{Name: "identity.expire-invitations", Every: 5 * time.Minute, Timeout: time.Minute,
			Run: func(ctx context.Context) (int, error) {
				return maintenance.ExpireDueInvitations(svc(ctx), maintenanceBatch)
			}},
		{Name: "identity.retry-tenant-bindings", Every: time.Minute, Timeout: time.Minute,
			Run: func(ctx context.Context) (int, error) {
				return maintenance.RetryTenantBindings(svc(ctx), maintenanceBatch)
			}},
		{Name: "identity.expire-approvals", Every: time.Minute, Timeout: time.Minute,
			Run: func(ctx context.Context) (int, error) {
				return maintenance.ExpireDueApprovals(svc(ctx), maintenanceBatch)
			}},
	}, nil
}
