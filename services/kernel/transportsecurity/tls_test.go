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

func TestDevelopmentUsesMutualTLSWhenConfigured(t *testing.T) {
	t.Setenv("MIAOQIA_ENV", "development")
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", "")
	if !transportsecurity.Plaintext() {
		t.Fatal("development without certificates should stay plaintext")
	}
	if rt, err := transportsecurity.Client(); err != nil || rt != http.DefaultTransport {
		t.Fatalf("plaintext client = %v err=%v", rt, err)
	}
	f := testenv.NewTLS(t)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CERT", f.CertPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_KEY", f.KeyPath)
	t.Setenv("MIAOQIA_INTERNAL_TLS_CA", f.CA)
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/other")
	if transportsecurity.Plaintext() {
		t.Fatal("configured certificates must disable plaintext")
	}
	rt, err := transportsecurity.Client()
	if err != nil {
		t.Fatal(err)
	}
	if transport, ok := rt.(*http.Transport); !ok || transport.TLSClientConfig == nil || len(transport.TLSClientConfig.Certificates) != 1 {
		t.Fatalf("configured client is not mTLS: %#v", rt)
	}
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
	// TLS requests enforce the allowlist even in development.
	response, err := (&http.Client{Transport: rt}).Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("unallowlisted development TLS peer status = %d", response.StatusCode)
	}
	t.Setenv("MIAOQIA_INTERNAL_ALLOWED_PEERS", "spiffe://miaoqia/service/platform-api")
	response, err = (&http.Client{Transport: rt}).Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("allowlisted development TLS peer status = %d", response.StatusCode)
	}
}
