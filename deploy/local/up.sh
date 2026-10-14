#!/usr/bin/env bash
# 启动本地基础组件并等待就绪，然后创建 Kafka 主题。用法：deploy/local/up.sh [--down]
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
