#!/usr/bin/env bash
# Run M0 E2E with the local PostgreSQL and the VPS public services.
# The temporary Kafka tunnel is created and removed by this wrapper.
set -euo pipefail

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
key_path=${MIAOQIA_VPS_SSH_KEY:-$HOME/.SSH/id_rsa}
ssh_user=${MIAOQIA_VPS_SSH_USER:-almalinux}
ssh_host=${MIAOQIA_VPS_HOST:-15.204.173.155}
local_port=${MIAOQIA_KAFKA_TUNNEL_PORT:-29094}

if ! command -v nc >/dev/null 2>&1; then
  echo "nc is required to verify the Kafka tunnel port" >&2
  exit 1
fi
if nc -z 127.0.0.1 "$local_port" >/dev/null 2>&1; then
  echo "local Kafka tunnel port ${local_port} is already in use; choose MIAOQIA_KAFKA_TUNNEL_PORT or stop the existing listener" >&2
  exit 1
fi

ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
  -i "$key_path" -L "${local_port}:127.0.0.1:29094" "$ssh_user@$ssh_host" &
tunnel_pid=$!
cleanup() { kill "$tunnel_pid" 2>/dev/null || true; wait "$tunnel_pid" 2>/dev/null || true; }
trap cleanup EXIT INT TERM
ready=0
for _ in $(seq 1 20); do
  if ! kill -0 "$tunnel_pid" 2>/dev/null; then
    break
  fi
  if nc -z 127.0.0.1 "$local_port" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.5
done
if [ "$ready" -ne 1 ]; then
  echo "Kafka SSH tunnel did not become ready on 127.0.0.1:${local_port}" >&2
  exit 1
fi

cd "$root_dir"
MIAOQIA_KAFKA_BROKERS="127.0.0.1:${local_port}" \
  MIAOQIA_ENV_FILE="${MIAOQIA_ENV_FILE:-$root_dir/config/local/m0-vps.env}" \
  uv run --project data --locked --all-packages python tests/e2e/m0.py "$@"
