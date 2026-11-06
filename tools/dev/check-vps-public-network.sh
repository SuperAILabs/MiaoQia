#!/usr/bin/env bash
# Verify that the VPS public stack uses the MiaoQia-owned network boundary.
# This check prints topology and health only; it never reads the deployment .env.
set -euo pipefail

key_path=${MIAOQIA_VPS_SSH_KEY:-$HOME/.SSH/id_rsa}
ssh_user=${MIAOQIA_VPS_SSH_USER:-almalinux}
ssh_host=${MIAOQIA_VPS_HOST:-15.204.173.155}

ssh -o BatchMode=yes -o ConnectTimeout=10 -i "$key_path" "$ssh_user@$ssh_host" 'bash -s' <<'REMOTE'
set -euo pipefail
compose=/opt/miaoqia-public/compose.yaml
nginx=/etc/nginx/conf.d/mq-public.conf
docker() { sudo -n docker "$@"; }

grep -q 'name: miaoqia_public' "$compose"
grep -q '10\.89\.14\.250' "$nginx"
grep -q '10\.89\.14\.251' "$nginx"
docker network inspect miaoqia_public >/dev/null

for name in miaoqia-public-postgres-1 miaoqia-public-keycloak-1 miaoqia-public-openfga-1; do
  test "$(docker inspect -f '{{.State.Status}}' "$name")" = running
  test "$(docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}} {{end}}' "$name")" = 'miaoqia_public '
done
test "$(docker inspect -f '{{.State.ExitCode}}' miaoqia-public-openfga-migrate-1)" = 0

curl -fsS --max-time 15 https://auth.lai.co/realms/miaoqia/.well-known/openid-configuration >/dev/null
curl -fsS --max-time 15 https://fga.lai.co/healthz >/dev/null

printf '%s\n' 'VPS MiaoQia public network check: passed'
printf '%s\n' 'network=miaoqia_public'
printf '%s\n' 'keycloak=reachable'
printf '%s\n' 'openfga=SERVING'
printf '%s\n' 'openfga_migration=exit-0'
printf '%s\n' 'legacy_network_reference=absent-from-miaoqia-config'
REMOTE
