#!/usr/bin/env bash
# Isolated integration-test certificates. Never use these for deployed services.
set -euo pipefail
m0_cert_dir="${1:?Pass a test output directory}"
mkdir -p "$m0_cert_dir"
chmod 700 "$m0_cert_dir"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$m0_cert_dir/ca.key" -out "$m0_cert_dir/ca.crt" -days 1 -subj '/CN=MiaoQia isolated M0 test CA' >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout "$m0_cert_dir/server.key" -out "$m0_cert_dir/server.csr" -subj '/CN=localhost' >/dev/null 2>&1
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' > "$m0_cert_dir/server.ext"
openssl x509 -req -in "$m0_cert_dir/server.csr" -CA "$m0_cert_dir/ca.crt" -CAkey "$m0_cert_dir/ca.key" -CAcreateserial -out "$m0_cert_dir/server.crt" -days 1 -extfile "$m0_cert_dir/server.ext" >/dev/null 2>&1
chmod 600 "$m0_cert_dir"/*
