#!/usr/bin/env bash
# One-time local setup for the shared VPS test services.
set -euo pipefail

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
key_path=${MIAOQIA_VPS_SSH_KEY:-$HOME/.SSH/id_rsa}
ssh_user=${MIAOQIA_VPS_SSH_USER:-almalinux}
ssh_host=${MIAOQIA_VPS_HOST:-15.204.173.155}
remote_env=$(ssh -i "$key_path" "$ssh_user@$ssh_host" 'sudo -n cat /opt/miaoqia-public/.env')

value() {
  printf '%s\n' "$remote_env" | awk -F= -v key="$1" '$1 == key { print substr($0, index($0, "=") + 1); exit }'
}

mkdir -p "$root_dir/config/local"
umask 077
{
  printf '%s\n' 'MIAOQIA_KAFKA_BROKERS=127.0.0.1:29094'
  printf '%s\n' 'MIAOQIA_IDENTITY_OPENFGA_URL=https://fga.lai.co'
  printf 'MIAOQIA_IDENTITY_OPENFGA_TOKEN=%s\n' "$(value FGA_TOKEN)"
  printf '%s\n' 'MIAOQIA_GATEWAY_ISSUER=https://auth.lai.co/realms/miaoqia'
  printf '%s\n' 'MIAOQIA_GATEWAY_AUDIENCE=miaoqia-api'
  printf '%s\n' 'MIAOQIA_E2E_CLIENT_ID=miaoqia-dev'
  printf 'MIAOQIA_E2E_OPERATOR_PASSWORD=%s\n' "$(value KC_OPERATOR_PASS)"
  printf 'MIAOQIA_E2E_VISITOR_PASSWORD=%s\n' "$(value KC_VISITOR_PASS)"
  printf '%s\n' 'OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.lai.co'
  printf '%s\n' 'MIAOQIA_E2E_TRACE_REQUIRED=false'
} > "$root_dir/config/local/m0-vps.env"
chmod 600 "$root_dir/config/local/m0-vps.env"
printf 'Wrote %s\n' "$root_dir/config/local/m0-vps.env"
