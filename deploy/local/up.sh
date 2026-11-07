#!/usr/bin/env bash
# 可选的本地基础组件引导脚本；开发验收默认复用共享组件，不自动调用本脚本。
# 用法：deploy/local/up.sh [--down]
set -euo pipefail
cd "$(dirname "$0")"
if [[ "${1:-}" == "--down" ]]; then
  docker compose down -v
  rm -rf .data
  exit 0
fi
mkdir -p .data/otel
docker compose up -d --wait
./topics.sh
echo "本地基础组件已就绪：PostgreSQL :5432、Temporal :7233、Kafka :9092、OpenSearch :9200、Valkey :6379、Keycloak :8080、OpenFGA :8081、OpenBao :8200、OTLP :4317/:4318"
