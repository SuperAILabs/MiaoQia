#!/usr/bin/env bash
set -euo pipefail
umask 077

image=mcr.microsoft.com/playwright/python@sha256:abf13b369f8829eb45e29df38d6c5221f7e7521649cb5d2de7989c82bdb574ad
seccomp_sha=cc3e61cabda6bbc1e53e54d27ba4d55a9d3be829b6dd1a596f4a7b31b1cc7849
nss_sha=d8d6093edcf2206edeee40eaddd90a54bacc84cde4d8cc9c4fb50bd7aa12cf0d
here=$(cd -- "$(dirname -- "$0")" && pwd)
cd -- "$here"
export PYTHONDONTWRITEBYTECODE=1

if [[ ${1:-} != --staged ]]; then
    mode=--full
    if [[ ${1:-} == --tls-pair || ${1:-} == --lifecycle || ${1:-} == --provider-ipc \
          || ${1:-} == --provider-role || ${1:-} == --provider-ingest ]]; then
        mode=$1
        shift
    fi
    stage=$(mktemp -d)
    trap 'rm -rf -- "$stage"' EXIT
    cp Containerfile publisher.py renderer.py chromium_home.py summarize.py verify-confinement.sh "$stage/"
    mkdir "$stage/lifecycle"
    cp lifecycle/{composition.py,composition_browser.py,supervisor.py,verify.py,probe.py} "$stage/lifecycle/"
    if [[ $mode == --provider-ipc || $mode == --provider-role || $mode == --provider-ingest ]]; then
        cp lifecycle/{composition_ipc.py,controller_ipc.py,driver_ipc.py,ipc_session.py} "$stage/lifecycle/"
    fi
    if [[ $mode == --provider-role || $mode == --provider-ingest ]]; then
        cp lifecycle/{composition_role.py,role_provider.py,role_archive.py} "$stage/lifecycle/"
    fi
    repo=$(cd ../../../../ && pwd)
    mkdir -p "$stage/deploy/acquisition"
    cp "$repo/deploy/subsystems.yaml" "$stage/deploy/"
    cp "$repo/deploy/acquisition/roles.yaml" "$stage/deploy/acquisition/"
    modules=(contracts sdk kernel subsystems/acquisition runtimes/acquisition)
    packages=(--package miaoqia-runtime-acquisition)
    if [[ $mode == --provider-ingest ]]; then
        cp lifecycle/{role_events.py,role_evidence.py} "$stage/lifecycle/"
        mkdir -p "$stage/deploy/"{data,platform}
        cp "$repo/deploy/data/roles.yaml" "$stage/deploy/data/"
        cp "$repo/deploy/platform/roles.yaml" "$stage/deploy/platform/"
        modules+=(subsystems/evidence subsystems/domain-intelligence subsystems/entity-graph runtimes/data)
        packages+=(--package miaoqia-runtime-data)
        (
            cd "$repo/services"
            export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local
            go env GOVERSION > "$stage/deploy/platform/go-version"
            sha256sum go.mod go.sum > "$stage/deploy/platform/go-locks.sha256"
            go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" \
                -o "$stage/deploy/platform/platform" ./cmd/platform
            sha256sum -c "$stage/deploy/platform/go-locks.sha256" >/dev/null
        )
    fi
    mkdir "$stage/miaoqia"
    for module in "${modules[@]}"; do
        cp -R "$repo/data/$module/src/miaoqia/." "$stage/miaoqia/"
    done
    find "$stage/miaoqia" -type d -name __pycache__ -prune -exec rm -rf {} +
    (
        cd "$repo/data"
        uv export --frozen "${packages[@]}" --no-emit-workspace --no-dev --no-annotate --no-header
    ) > "$stage/requirements.txt"
    cat playwright.requirements >> "$stage/requirements.txt"
    if [[ ${1:-} == --ssh && $# == 2 ]]; then
        COPYFILE_DISABLE=1 tar --no-xattrs -C "$stage" -cf - . | bash "$2" \
            'set -eu; umask 077; d=$(mktemp -d /var/tmp/mq-chromium.XXXXXXXX); trap '\''rm -rf -- "$d"'\'' EXIT; tar -xf - -C "$d"; bash "$d/verify-confinement.sh" --staged '"$mode"
    elif [[ $# == 0 ]]; then
        bash "$stage/verify-confinement.sh" --staged "$mode"
    else
        exit 64
    fi
    exit
fi
mode=${2:-}
[[ $# == 2 && ( $mode == --full || $mode == --tls-pair || $mode == --lifecycle
    || $mode == --provider-ipc || $mode == --provider-role || $mode == --provider-ingest ) ]] || exit 64

stage=platform_preflight
run=$(mktemp -d /var/tmp/mq-chromium-run.XXXXXXXX)
id=$(basename "$run" | tr '[:upper:]' '[:lower:]')
browser="$id-browser"
publisher="$id-publisher"
other="$id-other"
business="$id-business"
net="$id-net"
private_net="$id-private"
p=(podman --root "$run/graph" --runroot "$run/run")
ns=
receipt=
printf 'fixture_cleanup_ids %s %s\n' "$(basename -- "$here")" "$(basename -- "$run")" >&2
cleanup() {
    local code=$? removed=false
    local -a owned=()
    trap - EXIT
    if [[ -d $run/graph ]]; then
        mapfile -t owned < <(timeout 5 "${p[@]}" ps -aq --filter "label=io.miaoqia.fixture=$id")
        if (( ${#owned[@]} )); then
            timeout 20 "${p[@]}" rm --force --time 2 --ignore "${owned[@]}" >/dev/null 2>&1 || :
        fi
        timeout 20 "${p[@]}" network rm "$net" "$private_net" >/dev/null 2>&1 || :
    fi
    if [[ ! -d $run/graph ]] || (
        timeout 5 "${p[@]}" ps -a -q > "$run/containers" &&
        timeout 5 "${p[@]}" network ls --filter "label=io.miaoqia.fixture=$id" -q > "$run/networks" &&
        [[ ! -s $run/containers && ! -s $run/networks && ( -z $ns || ! -e $ns ) ]]
    ); then
        removed=true
    fi
    if [[ $removed == true && -d $run/graph ]]; then
        timeout 25 podman unshare rm -rf -- "$run" >/dev/null 2>&1 || removed=false
    elif [[ $removed == true ]]; then
        rm -rf -- "$run"
    fi
    if [[ -n $receipt ]]; then printf '%s\n' "$receipt"; fi
    if (( code != 0 )); then printf '{"kind":"incomplete","stage":"%s","exit":%d}\n' "$stage" "$code"; fi
    if [[ $removed != true || -e $run ]]; then
        printf '%s\n' '{"kind":"cleanup","verified":false}'
        exit 1
    fi
    printf '%s\n' '{"kind":"cleanup","containers":0,"networks":0,"namespace_removed":true,"temporary_storage_removed":true,"verified":true}'
    exit "$code"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

[[ $(uname -sm) == 'Linux x86_64' && $(podman info --format '{{.Host.Security.Rootless}} {{.Host.NetworkBackend}}') == 'true netavark' ]]
for tool in curl timeout python3 jq nsenter nft ip openssl sha256sum; do
    command -v "$tool" >/dev/null
done

stage=versions_and_build
curl --disable -fsS --max-time 30 https://archive.ubuntu.com/ubuntu/pool/main/n/nss/libnss3-tools_3.98-1build1_amd64.deb -o nss.deb 2>"$run/download.log"
curl --disable -fsS --max-time 30 https://raw.githubusercontent.com/microsoft/playwright/v1.60.0/utils/docker/seccomp_profile.json -o "$run/upstream.json" 2>>"$run/download.log"
printf '%s  %s\n' "$nss_sha" nss.deb "$seccomp_sha" "$run/upstream.json" | sha256sum -c --status
# Return ENOSYS for clone3 so glibc can fall back to the profile's filtered
# clone operation. Allow chroot for the Chromium zygote after it creates its
# own nested user namespace. The kernel still requires CAP_SYS_CHROOT there.
jq -e 'if any(.syscalls[]; .names | index("clone3")) then error("unexpected clone3 rule")
    else .syscalls += [{"names":["clone3"],"action":"SCMP_ACT_ERRNO","errnoRet":38}] end
    | if any(.syscalls[]; .names == ["chroot"] and .includes.caps == ["CAP_SYS_CHROOT"])
      then .syscalls[] |= (if .names == ["chroot"] and .includes.caps == ["CAP_SYS_CHROOT"] then .includes={} else . end)
      else error("expected upstream chroot rule") end' \
    "$run/upstream.json" > "$run/seccomp.json"
timeout 180 "${p[@]}" pull --quiet "$image" > "$run/pull.log" 2>&1
printf '%s\n' Containerfile miaoqia publisher.py renderer.py chromium_home.py lifecycle deploy requirements.txt nss.deb > "$run/context"
tar -cf "$run/context.tar" -T "$run/context"
sha256sum "$run/context.tar" | cut -d' ' -f1 > "$run/context.sha"
timeout 160 "${p[@]}" build --pull=never --layers=false --http-proxy=false \
    --tag "$id-image" -f "$here/Containerfile" "$here" > "$run/build.log" 2>&1 || {
    grep -oE 'No matching distribution found for [A-Za-z0-9_=.-]+|requires a different Python|not a valid build context|No such file or directory|Permission denied|connection refused|Temporary failure in name resolution|no space left on device' "$run/build.log" | sort -u >&2 || :
    exit 1
}
stage=offline_import
built=$("${p[@]}" image inspect --format '{{.Id}}' "$id-image")
"${p[@]}" run --rm --pull=never --network none --http-proxy=false "$built" \
    python -c 'from importlib.metadata import version; from miaoqia.runtime_acquisition.browser_renderer import PlaywrightBrowserRenderer; assert version("playwright") == "1.60.0"' > "$run/import.log" 2>&1
if [[ $mode == --provider-ingest ]]; then
    "${p[@]}" run --rm --pull=never --network none --http-proxy=false "$built" \
        python -c 'from miaoqia.runtime_data import modules; assert [entry.name for entry in modules()] == ["evidence", "domain-intelligence", "entity-graph"]' \
        > "$run/data-import.log" 2>&1
fi

stage=synthetic_pki
mkdir "$run/pki" "$run/keys"
chmod 755 "$run/pki"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$run/keys/ca.key" -out "$run/keys/ca.crt" -days 1 \
    -subj /CN=Fixture-CA -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign \
    > "$run/pki.log" 2>&1
openssl req -new -newkey rsa:2048 -nodes -keyout "$run/pki/leaf.key" -out "$run/keys/leaf.csr" \
    -subj /CN=publisher.fixture.test >> "$run/pki.log" 2>&1
printf '%s\n' 'subjectAltName=DNS:publisher.fixture.test' 'basicConstraints=critical,CA:FALSE' \
    'keyUsage=critical,digitalSignature,keyEncipherment' 'extendedKeyUsage=serverAuth' > "$run/keys/leaf.ext"
openssl x509 -req -in "$run/keys/leaf.csr" -CA "$run/keys/ca.crt" -CAkey "$run/keys/ca.key" \
    -CAcreateserial -days 1 -extfile "$run/keys/leaf.ext" -out "$run/pki/leaf.crt" >> "$run/pki.log" 2>&1
openssl verify -purpose sslserver -verify_hostname publisher.fixture.test -CAfile "$run/keys/ca.crt" \
    "$run/pki/leaf.crt" >> "$run/pki.log" 2>&1
chmod 444 "$run/pki/"*
rm "$run/keys/ca.key"

stage=network_setup
for name in "$net" "$private_net"; do
    "${p[@]}" network create --label "io.miaoqia.fixture=$id" --internal --disable-dns "$name" >/dev/null
done
address() {
    "${p[@]}" network inspect "$1" | jq -er '.[0] | select(.internal and (.ipv6_enabled|not) and (.dns_enabled|not))
        | .subnets[0].subnet' | python3 -c 'import ipaddress, sys; print(ipaddress.IPv4Network(sys.stdin.read().strip())[int(sys.argv[1])])' "$2"
}
origin_ip=$(address "$net" 50)
browser_ip=$(address "$net" 51)
other_ip=$(address "$net" 52)
bridge_first_ip=$(address "$net" 1)
business_ip=$(address "$private_net" 50)
common=(--pull=never --http-proxy=false --label "io.miaoqia.fixture=$id" --read-only --cap-drop ALL
    --security-opt no-new-privileges --pids-limit 200 --memory 768m --cpus 2 --log-driver k8s-file)
server=(--sysctl net.ipv4.ip_unprivileged_port_start=443)
publisher_mode=()
publisher_env=()
if [[ $mode == --lifecycle || $mode == --provider-ipc ]]; then publisher_mode=(--lifecycle); fi
if [[ $mode == --provider-role || $mode == --provider-ingest ]]; then
    publisher_mode=(--provider-role)
    python3 -c 'from datetime import datetime; from zoneinfo import ZoneInfo
print(datetime.now(ZoneInfo("Asia/Shanghai")).date().isoformat())' > "$run/published-on"
    publisher_env=(--env "MQ_FIXTURE_PUBLISHED_ON=$(cat "$run/published-on")")
fi
"${p[@]}" run -d "${common[@]}" "${server[@]}" --name "$publisher" --network "$net" --ip "$origin_ip" \
    --env "PUBLISHER_IP=$origin_ip" --env "BROWSER_IP=$browser_ip" "${publisher_env[@]}" \
    -v "$run/pki:/fixture-pki:ro,Z" "$built" python publisher.py "${publisher_mode[@]}" >/dev/null
"${p[@]}" run -d "${common[@]}" "${server[@]}" --name "$other" --network "$net" --ip "$other_ip" \
    --env "CANARY_IP=$other_ip" "$built" python publisher.py >/dev/null
"${p[@]}" run -d "${common[@]}" "${server[@]}" --name "$business" --network "$private_net" --ip "$business_ip" \
    --env "CANARY_IP=$business_ip" "$built" python publisher.py >/dev/null
for name in "$publisher" "$other" "$business"; do
    for (( i=0; i<25; i++ )); do
        "${p[@]}" logs "$name" > "$run/$name.log" 2>&1
        if [[ $name == "$publisher" ]]; then
            jq -es '([.[] | select(.kind == "canary_ready")] | length) == 2 and any(.[]; .kind=="https_ready")' \
                "$run/$name.log" >/dev/null && break
        else
            jq -es 'any(.[]; .kind == "canary_ready")' "$run/$name.log" >/dev/null && break
        fi
        sleep .2
    done
    (( i < 25 ))
done
browser_mode=()
browser_lifetime=160
if [[ $mode == --provider-ipc || $mode == --provider-role || $mode == --provider-ingest ]]; then
    browser_mode=(--init)
fi
if [[ $mode == --provider-role || $mode == --provider-ingest ]]; then browser_lifetime=560; fi
"${p[@]}" run -d "${common[@]}" "${browser_mode[@]}" --security-opt "seccomp=$run/seccomp.json" --name "$browser" --network "$net" \
    --ip "$browser_ip" --shm-size 128m "$built" sleep "$browser_lifetime" >/dev/null
"${p[@]}" inspect "$browser" | jq -e --arg net "$net" '
    .[0] | (.NetworkSettings.Networks | keys) == [$net] and
    ([.Mounts[]? | select(.Type == "volume" or .Type == "bind")] | length) == 0 and
    (.HostConfig.ReadonlyRootfs == true) and
    all(.Config.Env[]; test("^(MIAOQIA_|AWS_|.*(PROXY|SECRET|TOKEN|PASSWORD|DSN)=)"; "i") | not)
' >/dev/null

stage=kernel_policy
ns=$("${p[@]}" inspect --format '{{.NetworkSettings.SandboxKey}}' "$browser")
[[ -n $ns && -e $ns ]]
if [[ $mode == --lifecycle ]]; then
    stage=lifecycle
    command -v ss >/dev/null
    timeout --kill-after=5 45 python3 lifecycle/composition.py "$run" "$ns" "$browser_ip" "$origin_ip" \
        "$browser" "$publisher" "$built" "$(sha256sum "$run/seccomp.json" | cut -d' ' -f1)"
    exit
fi
if [[ $mode == --provider-ipc ]]; then
    stage=provider_ipc
    timeout --kill-after=5 80 python3 lifecycle/composition_ipc.py "$run" "$ns" "$browser_ip" "$origin_ip" \
        "$browser" "$publisher" "$built" "$(sha256sum "$run/seccomp.json" | cut -d ' ' -f1)"
    exit
fi
if [[ $mode == --provider-role || $mode == --provider-ingest ]]; then
    stage=provider_role
    role_options=()
    if [[ $mode == --provider-ingest ]]; then role_options=(--ingest); fi
    timeout --kill-after=5 600 python3 lifecycle/composition_role.py "$run" "$ns" "$browser_ip" "$origin_ip" \
        "$browser" "$publisher" "$built" "$private_net" "$(sha256sum "$run/seccomp.json" | cut -d' ' -f1)" \
        "${role_options[@]}"
    exit
fi
inside=("${p[@]}" unshare nsenter "--net=$ns")
[[ -z $("${inside[@]}" ip -4 route show default) && -z $("${inside[@]}" ip -6 route show default) ]]
cat > "$run/policy.nft" <<EOF
table inet mq_fixture {
  chain output {
    type filter hook output priority -150; policy drop;
    ip saddr $browser_ip ip daddr $origin_ip tcp dport 443 counter accept comment "pinned_https"
    meta nfproto ipv6 counter drop comment "ipv6"
    udp dport 53 counter drop comment "dns"
    meta l4proto udp counter drop comment "udp"
    ip daddr $origin_ip counter drop comment "other_port"
    counter drop comment "other_destination"
  }
  chain input {
    type filter hook input priority -150; policy drop;
    ct state established tcp sport 443 ip saddr $origin_ip ip daddr $browser_ip counter accept
  }
}
EOF
# Attest the rule is absent before installing it and present before any browser launch.
! "${inside[@]}" nft list table inet mq_fixture > /dev/null 2>&1
"${inside[@]}" nft -f - < "$run/policy.nft" > "$run/nft.log" 2>&1
"${inside[@]}" nft -j list table inet mq_fixture > "$run/before.json"
jq -e '
    [.nftables[].chain? | select(.policy == "drop") | .name] | sort | . == ["input","output"]
' "$run/before.json" >/dev/null

stage=renderer
render_rc=0
timeout 130 "${p[@]}" exec -i --env "PUBLISHER_IP=$origin_ip" --env "OTHER_IP=$other_ip" \
    --env "CANARY_IP=$business_ip" --env "BRIDGE_FIRST_IP=$bridge_first_ip" \
    "$browser" python renderer.py "$mode" < "$run/keys/ca.crt" > "$run/render.jsonl" 2>"$run/render.log" || render_rc=$?
"${inside[@]}" nft -j list table inet mq_fixture > "$run/after.json"
for name in "$publisher" "$other" "$business"; do
    "${p[@]}" logs "$name" > "$run/$name.log" 2>&1
done
stage=receipt
set +e
receipt=$(python3 summarize.py "$run" "$built" "$(podman version --format '{{.Client.Version}}')" \
    "$(sha256sum "$run/seccomp.json" | cut -d' ' -f1)" "$render_rc" "$publisher" "$other" "$business" "$mode")
verdict=$?
set -e
exit "$verdict"
