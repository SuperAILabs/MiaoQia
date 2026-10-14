// Package auth 校验 Keycloak 签发的访问令牌，并从令牌派生请求上下文（租户、主体）。
// 请求体、查询参数中的租户与主体一律不被信任。
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// Claims 是平台使用的令牌声明。
type Claims struct {
	jwt.RegisteredClaims
	Tenant string `json:"miaoqia_tenant,omitempty"` // 租户成员的令牌带租户；平台运营人员不带
	Cell   string `json:"miaoqia_cell,omitempty"`
}

// Verifier 校验令牌。
type Verifier struct {
	issuer string
	keys   keyfunc.Keyfunc
}

// NewVerifier 从签发方的 JWKS 加载公钥（后台定期刷新）。
func NewVerifier(ctx context.Context, issuer string) (*Verifier, error) {
	k, err := keyfunc.NewDefaultCtx(ctx, []string{strings.TrimSuffix(issuer, "/") + "/protocol/openid-connect/certs"})
	if err != nil {
		return nil, err
	}
	return &Verifier{issuer: issuer, keys: k}, nil
}

// ErrUnauthenticated 表示令牌缺失或无效。
var ErrUnauthenticated = errors.New("gateway: 未认证")

// Context 校验 Authorization 头中的令牌，返回请求上下文。
func (v *Verifier) Context(r *http.Request) (reqctx.Context, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return reqctx.Context{}, ErrUnauthenticated
	}
	var c Claims
	_, err := jwt.ParseWithClaims(raw, &c, v.keys.Keyfunc, jwt.WithIssuer(v.issuer), jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired())
	if err != nil || c.Subject == "" {
		return reqctx.Context{}, ErrUnauthenticated
	}
	return reqctx.Context{Tenant: c.Tenant, Cell: c.Cell, Purpose: "interactive",
		Principal: reqctx.Principal{ID: c.Subject, Kind: "user"}}, nil
}
