package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/superailabs/miaoqia/services/internal/testissuer"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
)

func TestSignedTokenBoundaries(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "production")
	issuer := testissuer.New(t)
	key, srv := issuer.Key, issuer.Server
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v, err := NewVerifier(ctx, srv.URL, "miaoqia-api")
	if err != nil {
		t.Fatal(err)
	}
	base := jwt.MapClaims{"typ": "Bearer", "iss": srv.URL, "aud": "miaoqia-api", "sub": "usr_test", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
	for _, c := range []struct {
		name, key string
		value     any
		valid     bool
	}{{"valid", "", nil, true}, {"wrong issuer", "iss", "other", false}, {"wrong audience", "aud", "other", false}, {"expired", "exp", time.Now().Add(-time.Minute).Unix(), false}, {"missing expiration", "exp", nil, false}, {"missing issued at", "iat", nil, false}, {"ID token", "typ", "ID", false}, {"missing token type", "typ", nil, false}, {"future nbf", "nbf", time.Now().Add(time.Hour).Unix(), false}, {"future iat", "iat", time.Now().Add(time.Hour).Unix(), false}, {"empty subject", "sub", "", false}, {"unpaired tenant", "miaoqia_tenant", "tnt_other", false}} {
		t.Run(c.name, func(t *testing.T) {
			claims := jwt.MapClaims{}
			for k, v := range base {
				claims[k] = v
			}
			if c.key != "" {
				if c.value == nil {
					delete(claims, c.key)
				} else {
					claims[c.key] = c.value
				}
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "test"
			raw, _ := token.SignedString(key)
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", "Bearer "+raw)
			_, err := v.Context(r)
			if (err == nil) != c.valid {
				t.Fatalf("valid=%v err=%v", c.valid, err)
			}
		})
	}
	wrongKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	forged.Header["kid"] = "test"
	bad, _ := forged.SignedString(wrongKey)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+bad)
	if _, err := v.Context(req); err == nil {
		t.Fatal("invalid signature accepted")
	}
	issuer.Rotate(wrongKey, "rotated")
	rotated := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	rotated.Header["kid"] = "rotated"
	raw, _ := rotated.SignedString(wrongKey)
	req.Header.Set("Authorization", "Bearer "+raw)
	if _, err := v.Context(req); err != nil {
		t.Fatalf("JWKS rotation not recovered: %v", err)
	}
	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS256, jwt.SigningMethodNone} {
		token := jwt.NewWithClaims(method, base)
		token.Header["kid"] = "test"
		signer := any([]byte("invalid-test-key"))
		if method == jwt.SigningMethodNone {
			signer = jwt.UnsafeAllowNoneSignatureType
		}
		raw, _ := token.SignedString(signer)
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		if _, err := v.Context(r); err == nil {
			t.Fatal("unexpected signing algorithm accepted")
		}
	}
}
func TestProductionIssuerRequiresHTTPSAndAudience(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "production")
	for _, c := range [][2]string{{"http://localhost", "api"}, {"https://localhost", ""}} {
		if _, err := NewVerifier(context.Background(), c[0], c[1]); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
}

func TestMFAAssurance(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "production")
	issuer := testissuer.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	verifier, err := NewVerifier(ctx, issuer.Server.URL, "miaoqia-api")
	if err != nil {
		t.Fatal(err)
	}
	for _, strong := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+issuer.Token(t, "user", "", "", strong))
		r.Header.Set("Miaoqia-MFA", "true")
		_, mfa, err := verifier.Verify(r)
		if err != nil || mfa != strong {
			t.Fatalf("signed assurance strong=%v got=%v err=%v", strong, mfa, err)
		}
	}
}

// 认证保证只取已签名的声明：amr=mfa、auth_time 与会话来源；未登记的会话来源为 unknown，不会被当作交互登录。
func TestAssuranceClaims(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "production")
	issuer := testissuer.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v, err := NewVerifier(ctx, issuer.Server.URL, "miaoqia-api")
	if err != nil {
		t.Fatal(err)
	}
	authTime := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	for _, c := range []struct {
		name   string
		claims map[string]any
		mfa    bool
		at     time.Time
		source string
	}{
		{"交互登录带认证时间", map[string]any{"amr": []string{"pwd", "mfa"}, "auth_time": authTime.Unix()}, true, authTime, reqctx.SourceIdP},
		{"只有 amr", map[string]any{"amr": []string{"mfa"}}, true, time.Time{}, reqctx.SourceIdP},
		{"免登会话", map[string]any{"auth_time": authTime.Unix(), "miaoqia_session_source": "channel"}, false, authTime, reqctx.SourceChannel},
		{"未知会话来源", map[string]any{"amr": []string{"mfa"}, "auth_time": authTime.Unix(), "miaoqia_session_source": "password"}, true, authTime, reqctx.SourceUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", "Bearer "+issuer.TokenWith(t, "usr_test", "", "", c.claims))
			_, a, err := v.VerifyAssurance(r)
			if err != nil || a.MFA != c.mfa || !a.AuthTime.Equal(c.at) || a.Source != c.source {
				t.Fatalf("%+v %v", a, err)
			}
		})
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+issuer.TokenWith(t, "usr_test", "", "", map[string]any{"auth_time": "yesterday"}))
	if _, _, err := v.VerifyAssurance(r); err == nil {
		t.Fatal("非数值 auth_time 必须拒绝")
	}
}
