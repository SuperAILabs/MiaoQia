package transportsecurity

import (
	"crypto/tls"
	"errors"
	"os"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// KafkaOptions loads one role's broker authentication without exposing credentials
// in startup errors. An entirely unset profile preserves isolated plaintext brokers.
func KafkaOptions() ([]kgo.Opt, error) {
	ca, hasCA := os.LookupEnv("MIAOQIA_KAFKA_TLS_CA")
	username, hasUsername := os.LookupEnv("MIAOQIA_KAFKA_SASL_USERNAME")
	password, hasPassword := os.LookupEnv("MIAOQIA_KAFKA_SASL_PASSWORD")
	mechanism, hasMechanism := os.LookupEnv("MIAOQIA_KAFKA_SASL_MECHANISM")
	useSASL := hasUsername || hasPassword || hasMechanism
	if !hasCA && !useSASL {
		return nil, nil
	}
	if ca == "" {
		return nil, errors.New("Kafka TLS CA configuration is required")
	}
	roots, err := Roots(ca)
	if err != nil {
		return nil, errors.New("Kafka TLS CA is unavailable or invalid")
	}
	options := []kgo.Opt{kgo.DialTLSConfig(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})}
	if !useSASL {
		return options, nil
	}
	if username == "" || password == "" {
		return nil, errors.New("Kafka SCRAM identity is incomplete")
	}
	auth := scram.Auth{User: username, Pass: password}
	switch mechanism {
	case "SCRAM-SHA-256":
		return append(options, kgo.SASL(auth.AsSha256Mechanism())), nil
	case "SCRAM-SHA-512":
		return append(options, kgo.SASL(auth.AsSha512Mechanism())), nil
	default:
		return nil, errors.New("Kafka SCRAM mechanism must be SCRAM-SHA-256 or SCRAM-SHA-512")
	}
}
