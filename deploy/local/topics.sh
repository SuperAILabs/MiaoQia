#!/usr/bin/env bash
# 创建本地 Kafka 主题（清单由 tools/dev/gen_local.py topics 从契约推导）。
set -euo pipefail
cd "$(dirname "$0")"
for t in $(python3 ../../tools/dev/gen_local.py topics); do
  docker compose exec -T kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 \
    --create --if-not-exists --topic "$t" --partitions 12 --replication-factor 1 >/dev/null
done
echo "Kafka 主题：$(python3 ../../tools/dev/gen_local.py topics | wc -l) 个"
