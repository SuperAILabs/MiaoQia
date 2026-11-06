#!/usr/bin/env bash
# Run the local acceptance boundary against the already-running shared services.
#
# config/local/m0-vps.env is not loaded by default because it is a VPS smoke-test
# file and may contain public service URLs and credentials. Set MIAOQIA_ENV_FILE
# explicitly when that or another shared-development env file is intended.
# Docker Compose is an explicit bootstrap fallback via --up, never the default.
set -euo pipefail

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
start_local=0
run_e2e=1
local_only=${MIAOQIA_ACCEPTANCE_LOCAL_ONLY:-false}
local_openfga=0
local_openfga_pid=""

usage() {
  cat <<'EOF'
Usage: tools/dev/run-local-acceptance.sh [--up] [--no-up] [--local-openfga] [--skip-e2e]

  --up       Bootstrap the optional local Docker Compose components first.
  --no-up    Reuse already-running shared services (the default).
  --local-openfga
             Start the installed OpenFGA binary on loopback with in-memory
             storage for this run. This avoids VPS network latency and does
             not require Docker. The binary is selected with
             MIAOQIA_OPENFGA_BIN (default: openfga); it is mutually exclusive
             with --up.
  --skip-e2e Run unit, integration, data and contract checks without tests/e2e/m0.py.
EOF
}

while (($#)); do
  case "$1" in
    --up) start_local=1 ;;
    --no-up) start_local=0 ;;
    --local-openfga) local_openfga=1 ;;
    --skip-e2e) run_e2e=0 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
  shift
done

if ((start_local && local_openfga)); then
  echo "--up 已包含 Compose OpenFGA，不能与 --local-openfga 同时使用；请二选一" >&2
  exit 2
fi

cleanup_local_openfga() {
  if [[ -n "$local_openfga_pid" ]] && kill -0 "$local_openfga_pid" 2>/dev/null; then
    kill "$local_openfga_pid" 2>/dev/null || true
    wait "$local_openfga_pid" 2>/dev/null || true
  fi
}
trap cleanup_local_openfga EXIT INT TERM

# If a shared-development env file is explicitly selected, load it for Go,
# Python and the E2E child process.  No environment file is selected by default.
if [[ -n "${MIAOQIA_ENV_FILE:-}" && -f "$MIAOQIA_ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  . "$MIAOQIA_ENV_FILE"
  set +a
fi
export MIAOQIA_ENV="${MIAOQIA_ENV:-development}"
export MIAOQIA_TEST_PG="${MIAOQIA_TEST_PG:-$("$root_dir/tools/dev/resolve-test-pg.sh")}"
export MIAOQIA_TEST_KAFKA="${MIAOQIA_TEST_KAFKA:-${MIAOQIA_KAFKA_BROKERS:-localhost:9092}}"
export MIAOQIA_TEST_OPENFGA="${MIAOQIA_TEST_OPENFGA:-${MIAOQIA_IDENTITY_OPENFGA_URL:-http://localhost:8081}}"
export MIAOQIA_IDENTITY_OPENFGA_URL="$MIAOQIA_TEST_OPENFGA"
export MIAOQIA_GATEWAY_ISSUER="${MIAOQIA_GATEWAY_ISSUER:-http://localhost:8080/realms/miaoqia}"
export MIAOQIA_E2E_TRACE_REQUIRED="${MIAOQIA_E2E_TRACE_REQUIRED:-true}"
export MIAOQIA_E2E_TRACES="${MIAOQIA_E2E_TRACES:-$root_dir/deploy/local/.data/otel/traces.jsonl}"

export MIAOQIA_COMMERCE_COVERAGE="${MIAOQIA_COMMERCE_COVERAGE:-INF-PROC=*}"

if ((local_openfga)); then
  openfga_bin=${MIAOQIA_OPENFGA_BIN:-openfga}
  if ! command -v "$openfga_bin" >/dev/null 2>&1; then
    echo "--local-openfga 需要可执行文件：$openfga_bin（可通过 MIAOQIA_OPENFGA_BIN 指定）" >&2
    exit 1
  fi
  openfga_http_addr=${MIAOQIA_LOCAL_OPENFGA_HTTP_ADDR:-127.0.0.1:8081}
  openfga_grpc_addr=${MIAOQIA_LOCAL_OPENFGA_GRPC_ADDR:-127.0.0.1:8082}
  openfga_metrics_addr=${MIAOQIA_LOCAL_OPENFGA_METRICS_ADDR:-127.0.0.1:2113}
  export MIAOQIA_TEST_OPENFGA="${MIAOQIA_LOCAL_OPENFGA_URL:-http://$openfga_http_addr}"
  export MIAOQIA_IDENTITY_OPENFGA_URL="$MIAOQIA_TEST_OPENFGA"
  # 本地服务关闭认证；避免显式选择 VPS env 时把远程 token 带入本地验收。
  unset MIAOQIA_IDENTITY_OPENFGA_TOKEN
  openfga_log=${MIAOQIA_LOCAL_OPENFGA_LOG:-$root_dir/deploy/local/.data/openfga/openfga.log}
  mkdir -p "$(dirname -- "$openfga_log")"
  "$openfga_bin" run \
    --datastore-engine memory \
    --http-addr "$openfga_http_addr" \
    --grpc-addr "$openfga_grpc_addr" \
    --metrics-addr "$openfga_metrics_addr" \
    --playground-enabled=false \
    --log-level "${MIAOQIA_LOCAL_OPENFGA_LOG_LEVEL:-warn}" \
    >"$openfga_log" 2>&1 &
  local_openfga_pid=$!
  openfga_ready=0
  for _ in $(seq 1 40); do
    if curl -fsS --max-time 1 "$MIAOQIA_TEST_OPENFGA/healthz" >/dev/null 2>&1; then
      openfga_ready=1
      break
    fi
    if ! kill -0 "$local_openfga_pid" 2>/dev/null; then
      break
    fi
    sleep 0.25
  done
  if ((openfga_ready == 0)); then
    echo "本地 OpenFGA 未能在 $MIAOQIA_TEST_OPENFGA 就绪；日志：$openfga_log" >&2
    cat "$openfga_log" >&2 || true
    exit 1
  fi
fi

echo "共享组件验收边界：PostgreSQL=$MIAOQIA_TEST_PG Kafka=$MIAOQIA_TEST_KAFKA OpenFGA=$MIAOQIA_TEST_OPENFGA"
if ! python3 "$root_dir/tools/dev/check_tcp.py" --label Kafka --endpoints "$MIAOQIA_TEST_KAFKA"; then
  echo "请先连接共享 Kafka，或通过 MIAOQIA_ENV_FILE/MIAOQIA_TEST_KAFKA 显式指定地址；验收入口不会自动启动 Docker。" >&2
  exit 1
fi
if [[ "$local_only" == "true" ]]; then
  case "$MIAOQIA_TEST_OPENFGA" in
    http://localhost:*|http://127.0.0.1:*) ;;
    *) echo "本机验收拒绝非本机 OpenFGA：$MIAOQIA_TEST_OPENFGA" >&2; exit 1 ;;
  esac
  case "$MIAOQIA_TEST_PG" in
    postgres://*localhost:*/*|postgres://*127.0.0.1:*/*|postgresql://*localhost:*/*|postgresql://*127.0.0.1:*/*) ;;
    *) echo "本机验收拒绝非本机 PostgreSQL：$MIAOQIA_TEST_PG" >&2; exit 1 ;;
  esac
  case "$MIAOQIA_TEST_KAFKA" in
    localhost:*|127.0.0.1:*|localhost:*,*|127.0.0.1:*,*) ;;
    *) echo "本机验收拒绝非本机 Kafka：$MIAOQIA_TEST_KAFKA" >&2; exit 1 ;;
  esac
fi

if ((start_local)); then
  "$root_dir/deploy/local/up.sh"
fi

echo "[1/4] Go 竞态测试"
go -C "$root_dir/services" test -race ./...

echo "[2/4] Python 数据面测试"
(cd "$root_dir/data" && uv run --locked --all-packages --group dev pytest -q kernel/tests subsystems \
  sdk/tests ../packs/information_domains/procurement/tests ../packs/information_domains/organizations/tests)

echo "[3/4] 契约检查与生成一致性"
uv run --project "$root_dir/tools/contracts" --python 3.12 --locked mqc check
uv run --project "$root_dir/tools/contracts" --python 3.12 --locked mqc generate --check

if ((run_e2e)); then
  echo "[4/4] M0 本地端到端验证"
  uv run --project "$root_dir/data" --locked --all-packages python "$root_dir/tests/e2e/m0.py"
else
  echo "[4/4] 已跳过 M0 本地端到端验证"
fi

echo "本地验收通过"
