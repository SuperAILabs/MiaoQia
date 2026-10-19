// Package testissuer shares the rotating HTTPS JWKS fixture used by token and lifecycle tests.
// It signs real JWTs but does not simulate an IdP login or an MFA ceremony.
package testissuer

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"github.com/golang-jwt/jwt/v5"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type Issuer struct {
	Server *httptest.Server
	Key    *rsa.PrivateKey
	mu     sync.RWMutex
	kid    string
}

func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &Issuer{Key: key, kid: "test"}
	i.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.mu.RLock()
		defer i.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": i.kid, "n": base64.RawURLEncoding.EncodeToString(i.Key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(i.Key.E)).Bytes())}}})
	}))
	t.Cleanup(i.Server.Close)
	ca := filepath.Join(t.TempDir(), "issuer-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.Server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIAOQIA_GATEWAY_CA_FILE", ca)
	return i
}
func (i *Issuer) Rotate(key *rsa.PrivateKey, kid string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Key = key
	i.kid = kid
}

// Token signs an access token. mfa=true models a fresh interactive login with a second factor: amr=mfa and auth_time=now.
func (i *Issuer) Token(t testing.TB, subject, tenant, cell string, mfa bool) string {
	t.Helper()
	extra := map[string]any{}
	if mfa {
		extra["amr"] = []string{"mfa"}
		extra["auth_time"] = time.Now().Unix()
	}
	return i.TokenWith(t, subject, tenant, cell, extra)
}

// TokenWith signs an access token with additional claims (for example amr, auth_time, miaoqia_session_source);
// a nil value removes the claim. iat/exp are always the current time, as for a token obtained by refresh.
func (i *Issuer) TokenWith(t testing.TB, subject, tenant, cell string, extra map[string]any) string {
	t.Helper()
	i.mu.RLock()
	defer i.mu.RUnlock()
	claims := jwt.MapClaims{"typ": "Bearer", "iss": i.Server.URL, "aud": "miaoqia-api", "sub": subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
	if tenant != "" {
		claims["miaoqia_tenant"] = tenant
	}
	if cell != "" {
		claims["miaoqia_cell"] = cell
	}
	for k, v := range extra {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = i.kid
	value, err := token.SignedString(i.Key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
