package api

import (
	"context"
	"time"

	"github.com/superailabs/miaoqia/services/kernel/binding"
)

// GuardBinding 是强认证门禁在绑定表中的名称。
const GuardBinding = "identity.Guard"

// Guard 是其他子系统执行强认证动作前调用的门禁（PRD 第1段 F3）。认证保证只在网关进程内传递（reqctx.Assurance），
// 因此只提供本地绑定；identity 拆为远程绑定时不提供该绑定，调用方取不到门禁即失败关闭。
//   - RequireStrongAuth：调用者是上下文租户的有效成员，且有15分钟内的强认证；
//   - RequireAdmin：在此之上还须有 can_manage_members（渠道安装与停用、强制解绑等管理员动作）。
//
// 不满足时返回 *Error：403 mfa_required（须重新完成强认证）、403 totp_enrollment_required（IdP 明确尚未登记 TOTP，
// 客户端先引导登记）、403 forbidden、403 invalid_tenant_context 或 503 identity_unavailable。调用方收到错误时不得执行原动作。
type Guard interface {
	RequireStrongAuth(ctx context.Context) error
	RequireAdmin(ctx context.Context) error
}

func LocalGuard(impl Guard) Guard { return localGuard{impl} }

type localGuard struct{ impl Guard }

func (g localGuard) RequireStrongAuth(ctx context.Context) error {
	_, err := binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RequireStrongAuth", Idempotent: true},
		func(ctx context.Context) (struct{}, error) { return struct{}{}, g.impl.RequireStrongAuth(ctx) })
	return err
}

func (g localGuard) RequireAdmin(ctx context.Context) error {
	_, err := binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RequireAdmin", Idempotent: true},
		func(ctx context.Context) (struct{}, error) { return struct{}{}, g.impl.RequireAdmin(ctx) })
	return err
}

// MaintenanceBinding 是 identity 定时维护任务在绑定表中的名称（本地绑定；定时任务角色随所属里程碑接入）。
const MaintenanceBinding = "identity.Maintenance"

// Maintenance 是 identity 的定时维护：
//   - ExpireDueInvitations 把到期的待接受邀请置为已过期（每条发布一次 identity.invitation.changed.v1），返回处理数量；
//   - RetryTenantBindings 重试接受邀请后尚未写入 IdP 的租户与 cell 归属，返回本次写入成功的数量。
//
// 两者都可重复执行，以服务主体的请求上下文调用。
type Maintenance interface {
	ExpireDueInvitations(ctx context.Context, limit int) (int, error)
	RetryTenantBindings(ctx context.Context, limit int) (int, error)
}

func LocalMaintenance(impl Maintenance) Maintenance { return localMaintenance{impl} }

type localMaintenance struct{ impl Maintenance }

func (m localMaintenance) ExpireDueInvitations(ctx context.Context, limit int) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "ExpireDueInvitations", Timeout: 30 * time.Second},
		func(ctx context.Context) (int, error) { return m.impl.ExpireDueInvitations(ctx, limit) })
}

func (m localMaintenance) RetryTenantBindings(ctx context.Context, limit int) (int, error) {
	return binding.Invoke(ctx, binding.Local, binding.Op{Subsystem: Name, Name: "RetryTenantBindings", Timeout: 30 * time.Second},
		func(ctx context.Context) (int, error) { return m.impl.RetryTenantBindings(ctx, limit) })
}
