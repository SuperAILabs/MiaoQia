#!/usr/bin/env bash
# Run the A02 L1 gate with one real process for each required role.
# Shared PostgreSQL/Kafka are prerequisites; Docker is never started.
set -euo pipefail

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
local_openfga=0
while (($#)); do
  case "$1" in
    --local-openfga) local_openfga=1 ;;
    -h|--help)
      sed -n '1,8p' "$0"
      echo "Usage: tools/dev/run-a02-process-replay.sh [--local-openfga]"
      exit 0
      ;;
    *) echo "Usage: tools/dev/run-a02-process-replay.sh [--local-openfga]" >&2; exit 2 ;;
  esac
  shift
done

export MIAOQIA_ENV="${MIAOQIA_ENV:-development}"
export MIAOQIA_TEST_PG="${MIAOQIA_TEST_PG:-$("$root_dir/tools/dev/resolve-test-pg.sh")}"
# 与 run-local-acceptance 保持一致：开发机默认复用本机共享 Kafka；VPS 隧道或 CI
# 通过 MIAOQIA_KAFKA_BROKERS / MIAOQIA_TEST_KAFKA 显式覆盖。脚本本身不启动 Docker。
export MIAOQIA_KAFKA_BROKERS="${MIAOQIA_KAFKA_BROKERS:-${MIAOQIA_TEST_KAFKA:-localhost:9092}}"
python3 "$root_dir/tools/dev/check_tcp.py" --label Kafka --endpoints "$MIAOQIA_KAFKA_BROKERS" || {
  echo "请先连接共享 Kafka，或通过 MIAOQIA_KAFKA_BROKERS/MIAOQIA_TEST_KAFKA 显式指定地址；进程回放不会自动启动 Docker。" >&2
  exit 1
}

fga_pid=""
cleanup() {
  if [[ -n "$fga_pid" ]] && kill -0 "$fga_pid" 2>/dev/null; then
    kill "$fga_pid" 2>/dev/null || true
    wait "$fga_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

if ((local_openfga)); then
  command -v "${MIAOQIA_OPENFGA_BIN:-openfga}" >/dev/null 2>&1 || { echo "未找到 openfga" >&2; exit 1; }
  export MIAOQIA_IDENTITY_OPENFGA_URL="http://127.0.0.1:8081"
  openfga_log="$root_dir/deploy/local/.data/openfga/a02-process-replay.log"
  mkdir -p "$(dirname -- "$openfga_log")"
  "${MIAOQIA_OPENFGA_BIN:-openfga}" run --datastore-engine memory --http-addr 127.0.0.1:8081 \
    --grpc-addr 127.0.0.1:8082 --metrics-addr 127.0.0.1:2113 --playground-enabled=false --log-level warn \
    >"$openfga_log" 2>&1 &
  fga_pid=$!
  for _ in $(seq 1 40); do
    curl -fsS --max-time 1 "$MIAOQIA_IDENTITY_OPENFGA_URL/healthz" >/dev/null 2>&1 && break
    sleep 0.25
  done
  curl -fsS --max-time 2 "$MIAOQIA_IDENTITY_OPENFGA_URL/healthz" >/dev/null
fi

cd "$root_dir"
exec uv run --project data --locked --all-packages python tests/e2e/a02_process_replay.py
