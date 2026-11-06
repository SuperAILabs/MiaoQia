#!/usr/bin/env bash
# 创建配置中 Kafka 的主题（清单由 tools/dev/gen_local.py 从契约推导）。
set -euo pipefail
cd "$(dirname "$0")/../.."
exec uv run -q --project data --locked --package miaoqia-kernel python tools/dev/ensure_topics.py
