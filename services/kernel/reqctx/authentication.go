package reqctx

import (
	"context"
	"time"
)

type assuranceKey struct{}

// 会话来源（Assurance.Source）。
const (
	SourceIdP     = "idp"     // 经 IdP 交互登录（密码，已登记的再加 TOTP）签发的令牌
	SourceChannel = "channel" // 渠道内置浏览器免登会话：不带强认证（PRD 第1段 F7 第6步）
	SourceUnknown = "unknown" // 令牌声明了未登记的会话来源：不满足任何强认证要求
)

// Assurance 是网关从已验证访问令牌取得的认证保证：
//   - MFA：令牌的 amr 含 mfa；
//   - AuthTime：令牌的 auth_time，即 IdP 签发的最近一次交互认证时间（刷新令牌不改变它）；缺省为零值；
//   - Source：会话来源，见 SourceIdP 等。
type Assurance struct {
	MFA      bool
	AuthTime time.Time
	Source   string
}

// WithAssurance carries assurance from the verified access token within the gateway process.
// It is deliberately NOT serialized in request-context headers: internal peers cannot assert MFA or auth time.
func WithAssurance(ctx context.Context, a Assurance) context.Context {
	return context.WithValue(ctx, assuranceKey{}, a)
}

// AssuranceOf returns the assurance validated by the public gateway (zero value when absent).
func AssuranceOf(ctx context.Context) Assurance {
	value, _ := ctx.Value(assuranceKey{}).(Assurance)
	return value
}

// WithMFA carries signed MFA assurance of an interactive IdP session without an authentication time.
func WithMFA(ctx context.Context, verified bool) context.Context {
	return WithAssurance(ctx, Assurance{MFA: verified, Source: SourceIdP})
}

// MFA reports signed MFA assurance validated by the public gateway.
func MFA(ctx context.Context) bool { return AssuranceOf(ctx).MFA }
