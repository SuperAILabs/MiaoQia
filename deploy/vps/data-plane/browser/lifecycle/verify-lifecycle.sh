#!/usr/bin/env bash
set -euo pipefail
umask 077

here=$(cd -- "$(dirname -- "$0")" && pwd)
inputs=(supervisor.py probe.py verify.py verify-lifecycle.sh)

if [[ ${1:-} == --staged ]]; then
    [[ $# == 3 && $2 =~ ^[a-f0-9]{16}$ && ( $3 == run || $3 == cleanup ) ]] || exit 64
    cd -- "$here"
    if [[ $3 == run ]]; then
        run=/var/tmp/mq-browser-lifecycle-$2
        mkdir -- "$run"
        printf '%s\n' "mq-browser-lifecycle-$2" > "$run/owner"
    fi
    exec timeout -k 5 280 python3 -B verify.py "$3" "$2"
fi

mode=run
if [[ ${1:-} == --cleanup ]]; then
    [[ $# -ge 2 && $2 =~ ^[a-f0-9]{16}$ ]] || exit 64
    mode=cleanup
    run_id=$2
    shift 2
else
    run_id=$(openssl rand -hex 8)
fi

if [[ ${1:-} == --ssh && $# == 2 ]]; then
    printf 'Lifecycle recovery token: %s\n' "$run_id" >&2
    COPYFILE_DISABLE=1 tar --no-xattrs -C "$here" -cf - "${inputs[@]}" | bash "$2" \
        'set -eu; umask 077; d=$(mktemp -d /var/tmp/mq-lifecycle-input.XXXXXXXX); trap '\''rm -rf -- "$d"'\'' EXIT; tar -xf - -C "$d"; bash "$d/verify-lifecycle.sh" --staged '"$run_id $mode"
elif [[ $# == 0 && $(uname -s) == Linux ]]; then
    printf 'Lifecycle recovery token: %s\n' "$run_id" >&2
    bash "$here/verify-lifecycle.sh" --staged "$run_id" "$mode"
else
    exit 64
fi
