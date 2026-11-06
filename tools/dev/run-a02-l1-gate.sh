#!/usr/bin/env bash
# Run the repeatable A02 service-side acceptance gate.
# The final process-role replay remains a separate L1-4 gate; this command
# covers the complete Go service path and its negative controls.
set -euo pipefail

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
local_openfga=0
case "${1:-}" in
  --local-openfga) local_openfga=1; shift ;;
  "") ;;
  *) echo "Usage: tools/dev/run-a02-l1-gate.sh [--local-openfga]" >&2; exit 2 ;;
esac
if (($#)); then
  echo "Usage: tools/dev/run-a02-l1-gate.sh [--local-openfga]" >&2
  exit 2
fi

export MIAOQIA_ENV="${MIAOQIA_ENV:-development}"
export MIAOQIA_TEST_PG="${MIAOQIA_TEST_PG:-$("$root_dir/tools/dev/resolve-test-pg.sh")}"
export MIAOQIA_TEST_OPENFGA="${MIAOQIA_TEST_OPENFGA:-${MIAOQIA_IDENTITY_OPENFGA_URL:-http://localhost:8081}}"
export MIAOQIA_IDENTITY_OPENFGA_URL="$MIAOQIA_TEST_OPENFGA"
export MIAOQIA_TEST_KAFKA="${MIAOQIA_TEST_KAFKA:-${MIAOQIA_KAFKA_BROKERS:-localhost:9092}}"
python3 "$root_dir/tools/dev/check_tcp.py" --label Kafka --endpoints "$MIAOQIA_TEST_KAFKA" || {
  echo "请先连接共享 Kafka，或通过 MIAOQIA_KAFKA_BROKERS/MIAOQIA_TEST_KAFKA 显式指定地址；服务端门禁不会自动启动 Docker。" >&2
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
  command -v "${MIAOQIA_OPENFGA_BIN:-openfga}" >/dev/null 2>&1 || {
    echo "--local-openfga 需要可执行文件 openfga" >&2
    exit 1
  }
  export MIAOQIA_TEST_OPENFGA="${MIAOQIA_LOCAL_OPENFGA_URL:-http://127.0.0.1:8081}"
  export MIAOQIA_IDENTITY_OPENFGA_URL="$MIAOQIA_TEST_OPENFGA"
  unset MIAOQIA_IDENTITY_OPENFGA_TOKEN
  openfga_bin=${MIAOQIA_OPENFGA_BIN:-openfga}
  mkdir -p "$root_dir/deploy/local/.data/openfga"
  "$openfga_bin" run --datastore-engine memory --http-addr 127.0.0.1:8081 \
    --grpc-addr 127.0.0.1:8082 --metrics-addr 127.0.0.1:2113 \
    --playground-enabled=false --log-level warn \
    >"$root_dir/deploy/local/.data/openfga/a02-l1-gate.log" 2>&1 &
  fga_pid=$!
  for _ in $(seq 1 40); do
    curl -fsS --max-time 1 "$MIAOQIA_TEST_OPENFGA/healthz" >/dev/null 2>&1 && break
    sleep 0.25
  done
  curl -fsS --max-time 2 "$MIAOQIA_TEST_OPENFGA/healthz" >/dev/null
fi

artifact_dir="$root_dir/deploy/local/.data/a02-l1"
mkdir -p "$artifact_dir"
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
go -C "$root_dir/services" test -race -count=1 ./kernel/integration \
  -run '^(TestPolicySimulationApproval|TestSignalInboxFeedback|TestObjectPageEvidence|TestBriefingQuickActions|TestCoverage|TestInstantNotifications)$' \
  | tee "$artifact_dir/service-gate.log"
finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
cat >"$artifact_dir/result.json" <<EOF
{
  "started_at": "$started_at",
  "finished_at": "$finished_at",
  "openfga": "$MIAOQIA_TEST_OPENFGA",
  "postgres": "$MIAOQIA_TEST_PG",
  "service_gate": "passed",
  "scope": ["policy-simulation", "signal-inbox", "object-page-evidence", "briefing", "coverage", "instant-notifications"],
  "process_role_replay": "pending"
}
EOF
echo "A02 服务端验收门禁通过；结果：$artifact_dir/result.json"
