package testenv

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TLSFixture is a test-only CA; signed peers exercise real TLS verification.
type TLSFixture struct {
	Pool                  *x509.CertPool
	Cert                  tls.Certificate
	CA, CertPath, KeyPath string
	ca                    *x509.Certificate
	key                   *rsa.PrivateKey
	dir                   string
}

func NewTLS(t testing.TB) *TLSFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	raw, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	f := &TLSFixture{Pool: pool, CA: caPath, ca: ca, key: key, dir: dir}
	f.Cert, f.CertPath, f.KeyPath = f.Peer(t, "platform-api", false)
	return f
}
func (f *TLSFixture) Peer(t testing.TB, name string, expired bool) (tls.Certificate, string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := url.Parse("spiffe://miaoqia/service/" + name)
	end := time.Now().Add(time.Hour)
	if expired {
		end = time.Now().Add(-time.Minute)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, URIs: []*url.URL{id}, NotBefore: time.Now().Add(-time.Hour), NotAfter: end, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	raw, err := x509.CreateCertificate(rand.Reader, cert, f.ca, &key.PublicKey, f.key)
	if err != nil {
		t.Fatal(err)
	}
	cp, kp := filepath.Join(f.dir, name+".crt"), filepath.Join(f.dir, name+".key")
	if err := os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	return pair, cp, kp
}
