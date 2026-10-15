// Package transportsecurity enforces authenticated service transport independently of business context.
package transportsecurity

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"strings"
)

func Development() bool { return os.Getenv("MIAOQIA_ENV") == "development" }
func Roots(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.New("invalid trusted CA")
	}
	return pool, nil
}
func Config(prefix string, server bool) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(os.Getenv(prefix+"_CERT"), os.Getenv(prefix+"_KEY"))
	if err != nil {
		return nil, err
	}
	roots, err := Roots(os.Getenv(prefix + "_CA"))
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: roots}
	if server {
		cfg.ClientCAs = roots
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
func Peer(r *http.Request) bool {
	if Development() {
		return true
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	identities := r.TLS.PeerCertificates[0].URIs
	if len(identities) != 1 {
		return false
	}
	id := identities[0].String()
	if !strings.HasPrefix(id, "spiffe://miaoqia/service/") {
		return false
	}
	for _, allowed := range strings.Split(os.Getenv("MIAOQIA_INTERNAL_ALLOWED_PEERS"), ",") {
		if id == strings.TrimSpace(allowed) {
			return true
		}
	}
	return false
}
func Client() (http.RoundTripper, error) {
	if Development() {
		return http.DefaultTransport, nil
	}
	cfg, err := Config("MIAOQIA_INTERNAL_TLS", false)
	if err != nil {
		return nil, err
	}
	return &http.Transport{TLSClientConfig: cfg}, nil
}
