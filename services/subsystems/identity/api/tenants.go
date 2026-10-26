package api

import (
	"context"
	"fmt"

	"github.com/superailabs/miaoqia/services/kernel/binding"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// TenantsBinding 是租户枚举在绑定表中的名称（本地绑定，供 scheduler 角色的按租户周期任务使用；验收线 R-01）。
const TenantsBinding = "identity.Tenants"

// SchedulerPrincipal 是 scheduler 角色枚举租户时使用的服务主体。只有它可以调用 Tenants；
// 枚举得到的每个租户由各子系统的任务再以本子系统的服务主体与该租户的请求上下文调用自己的维护入口。
const SchedulerPrincipal = "svc:scheduler"

// MaxTenantsPage 是 ActiveTenants 每页的上限。
const MaxTenantsPage = 1000

// Tenants 是只读的租户枚举：
//   - ActiveTenants 返回指定 cell 中状态为 active 的租户 ID，按 ID 升序、严格大于 after（空为从头开始），至多 limit 个
//     （1–MaxTenantsPage）；返回少于 limit 个即已到末尾。已停用的租户与其他 cell 的租户不返回。
//
// 调用者须是不带租户上下文的 SchedulerPrincipal 服务主体，否则 403 forbidden；参数无效时 400 invalid_request；
// 目录不可用时 503 identity_unavailable。
type Tenants interface {
	ActiveTenants(ctx context.Context, cell, after string, limit int) ([]string, error)
}

func LocalTenants(impl Tenants) Tenants { return localTenants{impl} }

type localTenants struct{ impl Tenants }

func (l localTenants) ActiveTenants(ctx context.Context, cell, after string, limit int) ([]string, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ActiveTenants", Idempotent: true},
		func(ctx context.Context) ([]string, error) { return l.impl.ActiveTenants(ctx, cell, after, limit) })
}

// ForEachActiveTenant 以 SchedulerPrincipal 分页枚举 cell 中的 active 租户，对每个租户调用 fn，返回 fn 处理数量之和。
// 某个租户失败不挡住其他租户：继续处理，最后返回带失败租户数的错误；枚举本身失败或 ctx 取消时立即返回。
// fn 收到的 ctx 不带请求上下文，由调用方以本子系统的服务主体与该租户构造。
func ForEachActiveTenant(ctx context.Context, tenants Tenants, cell string, fn func(ctx context.Context, tenant string) (int, error)) (int, error) {
	sctx := reqctx.With(ctx, reqctx.Context{Principal: reqctx.Principal{ID: SchedulerPrincipal, Kind: "service"}})
	total, failed := 0, 0
	var first error
	for after := ""; ; {
		page, err := tenants.ActiveTenants(sctx, cell, after, MaxTenantsPage)
		if err != nil {
			return total, err
		}
		for _, tenant := range page {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			n, err := fn(ctx, tenant)
			total += n
			if err != nil {
				failed++
				if first == nil {
					first = err
				}
			}
		}
		if len(page) < MaxTenantsPage {
			break
		}
		after = page[len(page)-1]
	}
	if failed > 0 {
		return total, fmt.Errorf("%d 个租户失败：%w", failed, first)
	}
	return total, nil
}
