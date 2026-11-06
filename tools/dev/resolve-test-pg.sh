#!/usr/bin/env bash
# Resolve the shared-development PostgreSQL DSN without starting a container.
# CI and isolated environments should set MIAOQIA_TEST_PG explicitly.
set -euo pipefail

if [[ -n "${MIAOQIA_TEST_PG:-}" ]]; then
  printf '%s\n' "$MIAOQIA_TEST_PG"
  exit 0
fi

if command -v psql >/dev/null 2>&1; then
  local_user=${MIAOQIA_LOCAL_PG_USER:-$(id -un)}
  local_host=${MIAOQIA_LOCAL_PG_HOST:-127.0.0.1}
  local_port=${MIAOQIA_LOCAL_PG_PORT:-5432}
  if PGCONNECT_TIMEOUT=2 psql -w -h "$local_host" -p "$local_port" -U "$local_user" -d postgres -Atqc 'select 1' >/dev/null 2>&1; then
    printf 'postgres://%s@%s:%s/postgres\n' "$local_user" "$local_host" "$local_port"
    exit 0
  fi
fi

# Keep the CI-compatible default as the final fallback. It is only used when
# no already-running shared PostgreSQL can be authenticated locally.
printf '%s\n' 'postgres://postgres:postgres@localhost:5432/postgres'
