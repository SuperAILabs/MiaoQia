package transportsecurity_test

import (
	"crypto/tls"
	"os"
	"strings"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"

	"github.com/superailabs/miaoqia/services/kernel/internal/testenv"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
)

func TestKafkaTransportConfiguration(t *testing.T) {
	const (
		ca        = "MIAOQIA_KAFKA_TLS_CA"
		user      = "MIAOQIA_KAFKA_SASL_USERNAME"
		password  = "MIAOQIA_KAFKA_SASL_PASSWORD"
		mechanism = "MIAOQIA_KAFKA_SASL_MECHANISM"
		marker    = "fixture-secret-marker"
	)
	for _, key := range []string{ca, user, password, mechanism} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	fixture := testenv.NewTLS(t)
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
		fail bool
	}{
		{name: "isolated plaintext"},
		{name: "explicit empty CA", env: map[string]string{ca: ""}, fail: true},
		{name: "invalid CA", env: map[string]string{ca: fixture.KeyPath}, fail: true},
		{name: "TLS", env: map[string]string{ca: fixture.CA}, want: "TLS"},
		{name: "SCRAM requires TLS", env: map[string]string{user: "reader", password: marker, mechanism: "SCRAM-SHA-512"}, fail: true},
		{name: "partial identity", env: map[string]string{ca: fixture.CA, password: marker}, fail: true},
		{name: "unsupported mechanism", env: map[string]string{ca: fixture.CA, user: "reader", password: marker, mechanism: marker}, fail: true},
		{name: "SCRAM-256", env: map[string]string{ca: fixture.CA, user: "reader", password: marker, mechanism: "SCRAM-SHA-256"}, want: "SCRAM-SHA-256"},
		{name: "SCRAM-512", env: map[string]string{ca: fixture.CA, user: "reader", password: marker, mechanism: "SCRAM-SHA-512"}, want: "SCRAM-SHA-512"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			options, err := transportsecurity.KafkaOptions()
			if tc.fail {
				if err == nil || len(options) != 0 || strings.Contains(err.Error(), marker) {
					t.Fatal("unsafe broker configuration was not rejected cleanly")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			client, err := kgo.NewClient(options...)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			config := client.OptValue(kgo.DialTLSConfig).(*tls.Config)
			if tc.want == "" {
				if config != nil || len(options) != 0 {
					t.Fatal("fixture transport changed")
				}
				return
			}
			if config == nil || config.InsecureSkipVerify || config.RootCAs == nil || config.ServerName != "" {
				t.Fatal("broker TLS must verify its certificate and dialed host")
			}
			mechanisms := client.OptValue(kgo.SASL).([]sasl.Mechanism)
			if tc.want == "TLS" && len(mechanisms) != 0 {
				t.Fatal("TLS-only configuration supplied SASL")
			}
			if tc.want != "TLS" && (len(mechanisms) != 1 || mechanisms[0].Name() != tc.want) {
				t.Fatal("wrong SCRAM mechanism")
			}
		})
	}
}
