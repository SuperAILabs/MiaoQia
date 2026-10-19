package api

import (
	"context"
	"errors"
	"time"
)

// 以下是 identity 的出站端口：由组装代码按部署提供适配器（identity.ModuleWith）。生产 IdP（Keycloak 管理接口）与邮件渠道的
// 适配器尚未接入，缺省使用“未接入”实现：所有调用返回 ErrNotWired，依赖它们的命令失败关闭（503 identity_unavailable），
// 邀请邮件记为发送失败。不做任何外部调用。

// ErrNotWired 表示端口没有接入生产适配器。
var ErrNotWired = errors.New("identity：端口未接入生产适配器")

// ErrAccountNotFound 表示 IdP 中没有该主体的账号。
var ErrAccountNotFound = errors.New("identity：IdP 中没有该账号")

// Account 是 IdP 中账号的认证属性，只用于接受邀请与强认证判断，不作为租户成员资格的权威（权威在 identity 目录）。
type Account struct {
	Subject       string
	Email         string // 账号邮箱（任意大小写，比较时按小写）
	EmailVerified bool   // 邮箱已验证（新账号经邀请链接完成验证由 IdP 注册流程负责）
	TOTPEnrolled  bool   // 已登记 TOTP
}

// IdentityProvider 是 IdP 的账号查询与租户归属写入端口。
//   - Account 读取账号的已验证邮箱与 TOTP 登记状态；没有账号时返回 ErrAccountNotFound，IdP 不可用时返回其他错误；
//   - BindTenant 把账号的租户与 cell 归属写入 IdP（之后签发的令牌带租户）；对同一取值幂等，失败可重试。
type IdentityProvider interface {
	Account(ctx context.Context, subject string) (*Account, error)
	BindTenant(ctx context.Context, subject, tenant, cell string) error
}

// InvitationMessage 是一封邀请邮件。Token 是当前链接令牌原文，只在发送时存在于内存中，identity 只保存其摘要。
type InvitationMessage struct {
	InvitationID string
	Tenant       string
	TenantName   string
	Email        string
	Name         string
	Token        string
	Generation   int64 // 链接代次：重发加一
	ExpiresAt    time.Time
}

// Notifier 是邀请邮件的投递端口。返回 nil 表示已交给邮件渠道；返回错误表示发送失败（邀请仍有效，可重发）。
type Notifier interface {
	SendInvitation(ctx context.Context, m InvitationMessage) error
}

// NotWired 是缺省的端口实现：IdP 与邮件渠道未接入时使用，所有调用返回 ErrNotWired。
type NotWired struct{}

func (NotWired) Account(context.Context, string) (*Account, error)        { return nil, ErrNotWired }
func (NotWired) BindTenant(context.Context, string, string, string) error { return ErrNotWired }
func (NotWired) SendInvitation(context.Context, InvitationMessage) error  { return ErrNotWired }
