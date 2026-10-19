package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/subsystems/identity/api"
)

// 强认证时效（PRD 第1段 F3 第2步、第13节第3项）：管理动作要求当前会话在 StrongAuthWindow 内完成过带第二因素的交互认证。
// 新鲜度只看 IdP 签发的 auth_time，不看令牌的签发时间：刷新令牌沿用原 auth_time，不延长窗口；仅有 amr=mfa 而没有
// auth_time 不能证明新鲜度；渠道免登会话与未知会话来源一律不满足。
const (
	StrongAuthWindow = 15 * time.Minute
	// authTimeSkew 是 auth_time 晚于 identity 时钟的容许偏差；更晚的 auth_time 视为不可信。
	authTimeSkew = 5 * time.Second
)

func (d *Directory) now() time.Time {
	if d != nil && d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// businessNow 是写入的业务时间（过期时间、接受时间、事件生效时间），取整到秒：接口与事件的 date-time 以秒表示，
// 保存的值与对外给出的值一致。
func (d *Directory) businessNow() time.Time { return d.now().UTC().Truncate(time.Second) }

func (d *Directory) idp() api.IdentityProvider {
	if d == nil || d.IdP == nil {
		return api.NotWired{}
	}
	return d.IdP
}

func (d *Directory) notifier() api.Notifier {
	if d == nil || d.Notifier == nil {
		return api.NotWired{}
	}
	return d.Notifier
}

// freshStrongAuth 判断认证保证在 now 时是否满足强认证时效：IdP 交互会话、amr 含 mfa、auth_time 存在且
// 不晚于 now+authTimeSkew、距 now 不超过 StrongAuthWindow（含边界：第15分00秒仍满足，第15分01秒不满足）。
func freshStrongAuth(a reqctx.Assurance, now time.Time) bool {
	if a.Source != reqctx.SourceIdP || !a.MFA || a.AuthTime.IsZero() {
		return false
	}
	if a.AuthTime.After(now.Add(authTimeSkew)) {
		return false
	}
	return now.Sub(a.AuthTime) <= StrongAuthWindow
}

// strongAuth 核对调用者的强认证时效，不满足时给出客户端下一步：
//   - 403 totp_enrollment_required：IdP 明确该账号尚未登记 TOTP（先登记，登记完成即视为本次强认证）；
//   - 403 mfa_required：其余情况，包括已登记、令牌已证明登记过（amr=mfa）以及登记状态未知（IdP 不可用）。
//     它只表示“须重新完成强认证”，不声明已登记；未知不会被当作“未登记”或“已满足”。
//
// account 为调用方已读取的 IdP 账号（可为 nil，此时仅在需要区分登记状态时查询）。不满足时原动作不执行。
func (d *Directory) strongAuth(ctx context.Context, subject string, account *api.Account) error {
	a := reqctx.AssuranceOf(ctx)
	if freshStrongAuth(a, d.now()) {
		return nil
	}
	if a.MFA {
		return failure(403, "mfa_required")
	}
	if account == nil {
		if found, err := d.idp().Account(ctx, subject); err == nil {
			account = found
		}
	}
	if account != nil && !account.TOTPEnrolled {
		return failure(403, "totp_enrollment_required")
	}
	return failure(403, "mfa_required")
}

// RequireStrongAuth 是 api.Guard：调用者是上下文租户的有效成员，且有15分钟内的强认证。
func (d *Directory) RequireStrongAuth(ctx context.Context) error {
	rc, err := actor(ctx)
	if err != nil {
		return err
	}
	if rc.Tenant == "" {
		return failure(403, "invalid_tenant_context")
	}
	if err := d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := session(ctx, tx, rc.Tenant, rc.Principal.ID, rc.Cell)
		return err
	}); err != nil {
		return err
	}
	return d.strongAuth(ctx, rc.Principal.ID, nil)
}

// RequireAdmin 是 api.Guard：在 RequireStrongAuth 之上还须有 can_manage_members（渠道安装与停用、强制解绑等）。
func (d *Directory) RequireAdmin(ctx context.Context) error {
	return d.tx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := d.admin(ctx, tx)
		return err
	})
}
