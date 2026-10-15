package transportsecurity_test

import (
	"crypto/tls"
	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRealMutualTLS(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "production")
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	f := testenv.NewTLS(t)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !transportsecurity.Peer(r) {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(204)
	}))
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: f.Pool, Certificates: []tls.Certificate{f.Cert}}
	s.StartTLS()
	defer s.Close()
	unknown, _, _ := f.Peer(t, "unauthorized", false)
	expired, _, _ := f.Peer(t, "expired", true)
	other := testenv.NewTLS(t)
	for _, c := range []struct {
		name   string
		cert   *tls.Certificate
		status int
	}{{"valid", &f.Cert, 204}, {"missing", nil, 0}, {"wrong CA", &other.Cert, 0}, {"expired", &expired, 0}, {"unauthorized service", &unknown, 401}} {
		t.Run(c.name, func(t *testing.T) {
			cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.Pool}
			if c.cert != nil {
				cfg.Certificates = []tls.Certificate{*c.cert}
			}
			client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
			response, err := client.Get(s.URL)
			if c.status == 0 {
				if err == nil {
					response.Body.Close()
					t.Fatal("TLS should reject")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != c.status {
				t.Fatal(response.StatusCode)
			}
		})
	}
}
func TestProductionMissingConfigFailsClosed(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "production")
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", "")
	if _, err := transportsecurity.Config("MIAOQIA_INTERNAL_TLS", true); err == nil {
		t.Fatal("missing TLS accepted")
	}
}
