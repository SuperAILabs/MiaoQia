// dev-issuer is a loopback-only JWT issuer for process-level development gates.
// It deliberately has no user store: the caller supplies the subject, tenant,
// cell, and optional MFA assertion in the token request. It is never a product
// authentication service and is only used by tools/dev process replay scripts.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"flag"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "loopback listen address")
	issuer := flag.String("issuer", "http://127.0.0.1:18080/realms/dev", "issuer URL")
	flag.Parse()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	const kid = "miaoqia-dev-process"
	mux := http.NewServeMux()
	mux.HandleFunc("/protocol/openid-connect/certs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		subject, tenant, cell := r.FormValue("subject"), r.FormValue("tenant"), r.FormValue("cell")
		if subject == "" {
			subject = r.FormValue("username")
		}
		if subject == "" || (tenant == "") != (cell == "") {
			http.Error(w, "subject and tenant/cell are required", http.StatusBadRequest)
			return
		}
		now := time.Now()
		claims := jwt.MapClaims{"typ": "Bearer", "iss": strings.TrimRight(*issuer, "/"), "aud": "miaoqia-api", "sub": subject, "iat": now.Unix(), "exp": now.Add(15 * time.Minute).Unix()}
		if tenant != "" {
			claims["miaoqia_tenant"], claims["miaoqia_cell"] = tenant, cell
		}
		if r.FormValue("mfa") == "1" || r.FormValue("mfa") == "true" {
			claims["amr"], claims["auth_time"] = []string{"mfa"}, now.Unix()
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid
		raw, err := tok.SignedString(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": raw, "token_type": "Bearer", "expires_in": 900})
	})
	if err := http.ListenAndServe(*addr, mux); err != nil {
		panic(err)
	}
}
