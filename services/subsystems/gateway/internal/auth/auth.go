// Package auth 校验 Keycloak 签发的访问令牌，并从令牌派生请求上下文（租户、主体）。
// 请求体、查询参数中的租户与主体一律不被信任。
package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

// Claims 是平台使用的令牌声明。
type Claims struct {
	jwt.RegisteredClaims
	Tenant    string   `json:"miaoqia_tenant,omitempty"` // 租户成员的令牌带租户；平台运营人员不带
	Cell      string   `json:"miaoqia_cell,omitempty"`
	TokenType string   `json:"typ"`
	AMR       []string `json:"amr,omitempty"`
}

// Verifier 校验令牌。
type Verifier struct {
	issuer   string
	audience string
	keys     keyfunc.Keyfunc
}

// NewVerifier 从签发方的 JWKS 加载公钥（后台定期刷新）。
func NewVerifier(ctx context.Context, issuer, audience string) (*Verifier, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || audience == "" || (u.Scheme != "https" && !(os.Getenv("MIAOQIA_ENV") == "development" && u.Scheme == "http")) {
		return nil, errors.New("gateway: issuer/audience配置无效或生产签发方未使用HTTPS")
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if file := os.Getenv("MIAOQIA_GATEWAY_CA_FILE"); file != "" {
		pem, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid issuer CA")
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}
	}
	failOpen := false
	k, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{strings.TrimSuffix(issuer, "/") + "/protocol/openid-connect/certs"}, keyfunc.Override{Client: client, HTTPTimeout: 5 * time.Second, RateLimitWaitMax: time.Second, NoErrorReturnFirstHTTPReq: &failOpen})
	if err != nil {
		return nil, err
	}
	return &Verifier{issuer: issuer, audience: audience, keys: k}, nil
}

// ErrUnauthenticated 表示令牌缺失或无效。
var ErrUnauthenticated = errors.New("gateway: 未认证")

// Context 校验 Authorization 头中的令牌，返回请求上下文。
func (v *Verifier) Context(r *http.Request) (reqctx.Context, error) {
	rc, _, err := v.Verify(r)
	return rc, err
}

// Verify returns context and signed MFA assurance; roles are never trusted from the token.
func (v *Verifier) Verify(r *http.Request) (reqctx.Context, bool, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return reqctx.Context{}, false, ErrUnauthenticated
	}
	var c Claims
	_, err := jwt.ParseWithClaims(raw, &c, v.keys.Keyfunc, jwt.WithIssuer(v.issuer), jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(), jwt.WithAudience(v.audience), jwt.WithIssuedAt())
	if err != nil || c.Subject == "" || c.TokenType != "Bearer" || c.IssuedAt == nil {
		return reqctx.Context{}, false, ErrUnauthenticated
	}
	rc := reqctx.Context{Tenant: c.Tenant, Cell: c.Cell, Purpose: "interactive", Principal: reqctx.Principal{ID: c.Subject, Kind: "user"}}
	if err := rc.Validate(); err != nil {
		return reqctx.Context{}, false, ErrUnauthenticated
	}
	mfa := false
	for _, method := range c.AMR {
		if method == "mfa" {
			mfa = true
		}
	}
	return rc, mfa, nil
}
