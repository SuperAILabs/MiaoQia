#!/usr/bin/env bash
set -euo pipefail

: "${POSTGRES_USER:?POSTGRES_USER is required}"
: "${KC_DB_PASS:?KC_DB_PASS is required}"
: "${FGA_DB_PASS:?FGA_DB_PASS is required}"

psql_admin=(psql --username "$POSTGRES_USER" --dbname postgres -v ON_ERROR_STOP=1)

ensure_role() {
  local role=$1 password=$2
  if "${psql_admin[@]}" -tAc "SELECT 1 FROM pg_roles WHERE rolname='${role}'" | grep -q 1; then
    "${psql_admin[@]}" -v role_password="$password" <<SQL
ALTER ROLE ${role} LOGIN PASSWORD :'role_password';
SQL
  else
    "${psql_admin[@]}" -v role_password="$password" <<SQL
CREATE ROLE ${role} LOGIN PASSWORD :'role_password';
SQL
  fi
}

ensure_database() {
  local database=$1 owner=$2
  if ! "${psql_admin[@]}" -tAc "SELECT 1 FROM pg_database WHERE datname='${database}'" | grep -q 1; then
    "${psql_admin[@]}" -c "CREATE DATABASE ${database} OWNER ${owner}"
  fi
}

ensure_role miaoqia_keycloak "$KC_DB_PASS"
ensure_role miaoqia_fga "$FGA_DB_PASS"
ensure_database miaoqia_keycloak miaoqia_keycloak
ensure_database miaoqia_fga miaoqia_fga
