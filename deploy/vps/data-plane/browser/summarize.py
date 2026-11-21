"""Join independent browser, kernel and listener receipts without retaining locators."""

from collections import Counter
import json
from pathlib import Path
import sys

from publisher import CASES, TLS_PAIR_CASES, emit


def read_jsonl(path):
    try:
        lines = path.read_text().splitlines()
    except (OSError, UnicodeError):
        return [], False
    records = []
    valid = bool(lines)
    for line in lines:
        try:
            record = json.loads(line)
        except ValueError:
            valid = False
            continue
        if not isinstance(record, dict) or not isinstance(record.get("kind"), str):
            valid = False
            continue
        records.append(record)
    return records, valid


def main(root, image, podman, seccomp, render_rc, publisher, other, business, mode):
    root = Path(root)
    render, render_log_valid = read_jsonl(root / "render.jsonl")
    upstream, publisher_log_valid = read_jsonl(root / f"{publisher}.log")
    other_records, other_log_valid = read_jsonl(root / f"{other}.log")
    business_records, business_log_valid = read_jsonl(root / f"{business}.log")
    canaries = [*upstream, *other_records, *business_records]
    publisher_log_valid = (publisher_log_valid and any(entry["kind"] == "https_ready" for entry in upstream)
                           and not any(entry["kind"] == "listener_error" for entry in upstream))
    canary_logs_valid = other_log_valid and business_log_valid and all(
        any(entry["kind"] == "canary_ready" for entry in records) for records in (other_records, business_records)
    )
    counts = dict.fromkeys(("pinned_https", "ipv6", "dns", "udp", "other_port", "other_destination"), 0)
    table = json.loads((root / "after.json").read_text())
    for item in table["nftables"]:
        rule = item.get("rule", {})
        if rule.get("comment") in counts:
            counter, = [expr["counter"] for expr in rule["expr"] if "counter" in expr]
            counts[rule["comment"]] = counter["packets"]
    requests = [entry for entry in upstream if entry["kind"] == "request"]
    sni = Counter("matched" if entry["match"] else "wrong_name" if entry["wrong_name_control"] else "unexpected"
                  for entry in upstream if entry["kind"] == "sni")
    sockets = next((entry for entry in render if entry["kind"] == "socket_probes"), {})
    renders = {entry["case"]: entry for entry in render if entry["kind"] == "render"}
    arrivals = sum(entry["kind"] == "canary_arrival" for entry in canaries)
    socket_probes_passed = len(sockets) == 6 and all(value is True for key, value in sockets.items() if key != "kind")
    network_passed = (socket_probes_passed and all(counts.values()) and arrivals == 0
                      and publisher_log_valid and canary_logs_valid)
    emit(
        kind="build", base_image_digest="sha256:abf13b369f8829eb45e29df38d6c5221f7e7521649cb5d2de7989c82bdb574ad",
        image_id=image, staged_context_sha256=(root / "context.sha").read_text().strip(),
        playwright="1.60.0", podman=podman, seccomp_sha256=seccomp,
    )
    emit(
        kind="kernel", passed=bool(network_passed), packet_counts=counts, socket_probes=sockets,
        browser_bind_or_volume_mounts=0, browser_networks=1, synthetic_business_network_attached=False,
        namespace_default_routes=0, policy_before_chromium=True, rootless=True, read_only_rootfs=True,
        no_inherited_proxy_or_credential_env=True, canary_arrivals=arrivals,
    )
    emit(kind="tls_sni", **dict(sni))
    emit(kind="evidence_integrity", render_log_valid=render_log_valid, publisher_log_valid=publisher_log_valid,
         canary_logs_valid=canary_logs_valid,
         publisher_client_disconnects=sum(entry["kind"] == "client_disconnect" for entry in upstream))
    complete = (render_rc == "0" and render_log_valid and network_passed
                and sni["matched"] >= 2 and sni["wrong_name"] >= 1)
    denial = {"untrusted_ca": "ERR_CERT_AUTHORITY_INVALID", "wrong_name": "ERR_CERT_COMMON_NAME_INVALID"}
    expected_data = {
        "pair": {1}, "duplicate": {1}, "gzip": {1}, "worker": {0, 1},
        "prefetch": {0, 1}, "websocket": {0, 1}, "popup_view": {0, 1},
    }
    selected = TLS_PAIR_CASES if mode == "--tls-pair" else CASES
    for case in CASES:
        if case not in selected:
            emit(kind="case", case=case, executed=False, outside_run_scope=True)
            complete = complete and case not in renders and not any(item["case"] == case for item in requests)
            continue
        result = renders.get(case)
        if result is None:
            emit(kind="case", case=case, executed=False)
            complete = False
            continue
        saw = [item for item in requests if item["case"] == case]
        allowed_views = sum(item["request"] == "view" for item in saw)
        allowed_data = sum(item["request"] == "data" for item in saw)
        unexpected = sum(item["request"] == "unexpected" for item in saw)
        tls_peers = all(item["approved_peer"] and item["expected_client"] and item["tls"] in {"TLSv1.2", "TLSv1.3"}
                        for item in saw)
        expected_view = 0 if case in denial or case == "provider_veto" else 1
        expected_status = {"redirect": 302, "forbidden_status": 403, "throttled": 429}.get(case, 200)
        status_matches = all(item["status"] == expected_status for item in saw if item["request"] == "view")
        state = (result["returned_pair"] == (case in {"pair", "worker"}) and allowed_views == expected_view
                 and allowed_data in expected_data.get(case, {0}) and unexpected == 0 and tls_peers
                 and status_matches and result.get("tls_error") == denial.get(case)
                 and result.get("fresh_profile", False) and result.get("temporary_profiles_removed", False))
        extra = {}
        if case == "pair":
            origin_hashes = [item["body_sha256"] for item in saw if item["request"] == "data"]
            extra["original_hash_matches_publisher"] = origin_hashes == [result.get("original_sha256")]
            state = state and extra["original_hash_matches_publisher"] and all(
                result.get(check) for check in ("namespace_sandbox", "seccomp_bpf_sandbox", "same_navigation", "safe_command_line_verified")
            )
        if case == "worker":
            state = state and result.get("worker_registration") in {"blocked", "unavailable"}
        if case == "provider_veto":
            state = state and result.get("provider_veto_seen") is True
        if case in {"popup", "popup_view"}:
            extra["target_is_exact_approved_address"] = True
        state = state and render_log_valid and publisher_log_valid
        complete = complete and state
        result = {key: value for key, value in result.items() if key not in ("kind", "case")}
        emit(kind="case", case=case, executed=True, passed=bool(state),
             allowed_views=allowed_views if publisher_log_valid else None,
             allowed_data=allowed_data if publisher_log_valid else None,
             unexpected_origin_requests=unexpected if publisher_log_valid else None,
             publisher_observed_expected_peer_and_tls=tls_peers if saw else None, **result, **extra)
    unknown = sum(item["case"] == "unknown" for item in requests)
    emit(kind="result", renderer_exit=int(render_rc), unclassified_origin_requests=unknown,
         bounded_cases_passed=bool(complete and unknown == 0), full_case_set=mode == "--full",
         all_target_http_policy_verified=False)
    for error in render:
        if error["kind"] == "fixture_failure":
            print(json.dumps(error))
    return 0 if complete and unknown == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main(*sys.argv[1:]))
