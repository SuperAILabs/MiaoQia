"""Run the browser fixture under an independently revoked kernel lease."""

import base64
from contextlib import ExitStack
from hashlib import sha256
from ipaddress import IPv4Address
import json
from pathlib import Path
import select
import subprocess
import sys

from probe import emit
from supervisor import ControllerWatch, EgressLease, TABLE, command
from verify import require, stop


INJECTOR = """import signal, sys, time
time.sleep(0.4)
before = time.monotonic_ns()
signal.pidfd_send_signal(int(sys.argv[1]), signal.SIGKILL)
print(before, time.monotonic_ns())
"""


def record(worker, kind):
    require(select.select([worker.stdout], [], [], 8)[0], "worker response deadline")
    value = json.loads(worker.stdout.readline(8192))
    if value.get("kind") == "worker_failure":
        stages = [[file, line, name] for file, line, name in value.get("stack", [])
                  if file in {"composition_browser.py", "renderer.py"}]
        emit("worker_failure", error=value["error"], stack=stages)
    require(value.get("kind") == kind, "unexpected worker response")
    return value


def send(worker, message):
    worker.stdin.write(message + b"\n")


def browser_arrivals(records):
    return [entry for entry in records if entry["kind"] == "request"
            or (entry["kind"] in {"tcp_accept", "lifecycle_http"} and entry["from_browser"])]


def established(lease, client, peer, source_port):
    sockets = lease.run_inside("ss", "-4Hnt", "state", "established")
    endpoints = {tuple(line.split()[-2:]) for line in sockets.splitlines()}
    return (f"{client}:{source_port}", f"{peer}:443") in endpoints


def drill(run, namespace, client, peer, browser, publisher, image, seccomp_sha):
    root = Path(run)
    podman = ["podman", "--root", str(root / "graph"), "--runroot", str(root / "run")]
    client, peer = str(IPv4Address(client)), str(IPv4Address(peer))
    ca = base64.b64encode((root / "keys/ca.crt").read_bytes())
    worker_path = "/fixture/lifecycle/composition_browser.py"

    def origin_records():
        records = [json.loads(line) for line in command([*podman, "logs", publisher]).splitlines()]
        require(any(entry["kind"] == "https_ready" for entry in records)
                and all(entry["kind"] != "listener_error" for entry in records), "publisher logs invalid")
        return records

    def peer_health():
        command([*podman, "exec", "-i", publisher, "python", worker_path, "health"],
                script=ca.decode("ascii") + "\n", timeout=4)

    stage = "build_identity"
    inputs = ("Containerfile", "renderer.py", "publisher.py", "verify-confinement.sh", "lifecycle/supervisor.py",
              "lifecycle/verify.py", "lifecycle/probe.py", "lifecycle/composition.py", "lifecycle/composition_browser.py",
              "miaoqia/runtime_acquisition/browser_renderer.py")
    emit("build", image_id=image, seccomp_sha256=seccomp_sha,
         staged_context_sha256=(root / "context.sha").read_text().strip(),
         input_sha256={name: sha256(Path(name).read_bytes()).hexdigest() for name in inputs},
         podman=command(["podman", "version", "--format", "{{.Client.Version}}"]).strip())
    try:
        with ExitStack() as stack:
            stage = "install_lease"
            lease = EgressLease(podman, namespace, client, peer)
            stack.callback(lease.close)
            require(not lease.eligible(), stage)
            emit("isolation", rootless=True, browser_networks=1, namespace_default_routes=0,
                 browser_bind_or_volume_mounts=0, policy_installed_before_launch=True, initially_ineligible=True)
            peer_health()
            stage = "owned_controller_grant"
            controller = subprocess.Popen(
                [sys.executable, "-c", "import sys,time; print(sys.stdin.readline().strip(), flush=True); time.sleep(30)"],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0,
            )
            stack.callback(stop, controller)
            send(controller, b"authorize-synthetic-https")
            require(select.select([controller.stdout], [], [], 2)[0]
                    and controller.stdout.readline(64) == b"authorize-synthetic-https\n", stage)
            watch = ControllerWatch(lease, controller)
            stack.callback(watch.close)
            watch.grant()
            stack.callback(watch.revoke)
            stage = "chromium_pair_and_warmup"
            worker = subprocess.Popen(
                [*podman, "exec", "-i", "--env", f"PUBLISHER_IP={peer}", browser, "python", worker_path, "browser"],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0,
            )
            stack.callback(stop, worker)
            send(worker, ca)
            require(record(worker, "warm")["browser_alive"] is True, stage)
            before = origin_records()
            pair = [entry for entry in before if entry["kind"] == "request"]
            require([(entry["case"], entry["request"], entry["status"]) for entry in pair]
                    == [("pair", "view", 200), ("pair", "data", 200)]
                    and all(entry["approved_peer"] and entry["expected_client"] for entry in pair), stage)
            probes = [entry for entry in before if entry["kind"] == "lifecycle_http" and entry["from_browser"]]
            probe, = probes
            source_port = probe["port"]
            accepts = [entry for entry in before if entry["kind"] == "tcp_accept" and entry["from_browser"]]
            require(probe["allowed"] and probe["endpoint"] == "probe" and type(source_port) is int
                    and 1 <= source_port <= 65535 and sum(entry["port"] == source_port for entry in accepts) == 1
                    and established(lease, client, peer, source_port), stage)
            lease.nft(f"""add counter inet {TABLE} composed_old
insert rule inet {TABLE} output ip saddr {client} ip daddr {peer} ip daddr != @eligible \
 tcp sport {source_port} tcp dport 443 ct state established tcp flags & psh == psh counter name composed_old drop
""")

            def old_packets():
                payload = json.loads(lease.run_inside("nft", "-j", "list", "counter", "inet", TABLE, "composed_old"))
                counter, = (entry["counter"] for entry in payload["nftables"] if "counter" in entry)
                return counter["packets"]

            stage = "controller_sigkill"
            injector = subprocess.Popen([sys.executable, "-c", INJECTOR, str(watch.pidfd)], stdout=subprocess.PIPE,
                                        stderr=subprocess.DEVNULL, pass_fds=(watch.pidfd,))
            stack.callback(stop, injector)
            waiting, loss = watch.wait_and_revoke(include_observed_at=True)
            injection, _ = injector.communicate(timeout=2)
            send_started, send_returned = map(int, injection.split())
            observed = loss.pop("observed_at_ns")
            waiting_before_kill = injector.returncode == 0 and waiting < send_started <= observed
            loss.update(watchdog_wait_started_before_kill=waiting_before_kill, selector_scope="supervisor_owned_child",
                        signal_send_to_observation_upper_bound_us=(observed - send_started + 999) // 1000,
                        signal_syscall_duration_us=(send_returned - send_started + 999) // 1000,
                        exact_exit_timestamp_observed=False)
            emit("controller", **loss)
            require(all(loss[field] for field in ("controller_sigkill", "pidfd_exit_observed",
                        "revoked_before_lease_expiry", "watchdog_wait_started_before_kill")), stage)

            stage = "established_socket_probe"
            still_established = established(lease, client, peer, source_port)
            old_before = old_packets()
            send(worker, b"old")
            old = record(worker, "old")
            old_drops = old_packets() - old_before
            stage = "fresh_socket_probe"
            syn_before = lease.denied_packets(syn=True)
            send(worker, b"new")
            fresh = record(worker, "new")
            syn_drops = lease.denied_packets(syn=True) - syn_before
            stage = "independent_peer"
            peer_health()
            after = origin_records()
            healthy = [entry for entry in after if entry["kind"] == "lifecycle_http" and not entry["from_browser"]]
            require(after[:len(before)] == before and len(healthy) == 2 and all(
                entry["allowed"] and entry["endpoint"] == "health" for entry in healthy), stage)
            added = browser_arrivals(after[len(before):])
            emit("revocation", chromium_process_and_capture_context_alive=old["browser_alive"] and fresh["browser_alive"],
                 established_tuple_present_after_revoke=still_established, old_probe_timed_out=old["timed_out"],
                 new_probe_timed_out=fresh["timed_out"], established_tuple_psh_drops=old_drops, fresh_syn_drops=syn_drops,
                 origin_tcp_accepts_during_revocation_and_probes=sum(entry["kind"] == "tcp_accept" for entry in added),
                 origin_http_requests_during_revocation_and_probes=sum(entry["kind"] != "tcp_accept" for entry in added),
                 publisher_tls_health_checks=len(healthy))

            stage = "pair_and_profile_verification"
            send(worker, b"finish")
            result = record(worker, "render")
            require(worker.wait(timeout=5) == 0, stage)
            checks = {key: result.get(key) is True for key in (
                "returned_pair", "same_navigation", "both_cdp_peers_match_pin", "fresh_profile",
                "safe_command_line_verified", "namespace_sandbox", "seccomp_bpf_sandbox", "temporary_profiles_removed",
            )}
            tls_names = {"TLSv1.2": "TLS 1.2", "TLSv1.3": "TLS 1.3"}
            checks.update(permit_count=result["permit_completions"] == 2,
                          publisher_hash_matches=result.get("original_sha256") == pair[1]["body_sha256"],
                          publisher_tls_matches=result.get("tls") == [tls_names.get(entry["tls"]) for entry in pair])
            if not all(checks.values()):
                emit("pair_validation", **checks)
                raise RuntimeError(stage)
            result.pop("kind")
            emit("pair", publisher_peer_verified=True, publisher_tls_matches=True, publisher_hash_matches=True,
                 origin_gets=len(pair), **result)
            stage = "revocation_verdict"
            require(still_established and not added and old_drops > 0 and syn_drops > 0 and all(
                result["timed_out"] and result["browser_alive"] for result in (old, fresh)), stage)
    except Exception as error:
        from traceback import extract_tb

        emit("incomplete", stage=stage, error=type(error).__name__,
             location=[[Path(frame.filename).name, frame.lineno, frame.name]
                       for frame in extract_tb(error.__traceback__)[-4:]])
        return 1
    emit("result", bounded_composition_passed=True, all_target_http_policy_verified=False)
    return 0


if __name__ == "__main__":
    raise SystemExit(drill(*sys.argv[1:]))
