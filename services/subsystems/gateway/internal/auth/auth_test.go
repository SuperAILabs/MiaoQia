package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSignedTokenBoundaries(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "production")
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var mu sync.RWMutex
	currentKey := key
	currentID := "test"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": currentID, "n": base64.RawURLEncoding.EncodeToString(currentKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(currentKey.E)).Bytes())}}})
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600)
	t.Setenv("MIAOQIA_GATEWAY_CA_FILE", ca)
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
	mu.Lock()
	currentKey = wrongKey
	currentID = "rotated"
	mu.Unlock()
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
