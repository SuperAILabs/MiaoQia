"""Combine separate controller and Chromium containers with a supervised lease."""

import base64
from concurrent.futures import ThreadPoolExecutor
from hashlib import sha256
from ipaddress import IPv4Address
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import time
from traceback import extract_tb

from ipc_session import RevocationError, serve
from probe import emit
from supervisor import EgressLease, command
from verify import require, stop


def entries(output):
    records = [json.loads(line) for line in output.splitlines()]
    require(all(isinstance(record, dict) for record in records), "invalid fixture log")
    return records


def requests(records):
    return [record for record in records if record["kind"] == "request"]


def publisher_health_records(podman, publisher, ca):
    command([*podman, "exec", "-i", publisher, "python", "/fixture/lifecycle/composition_browser.py", "health"],
            script=ca + "\n", timeout=4)
    records = entries(command([*podman, "logs", publisher]))
    require(any(record["kind"] == "https_ready" for record in records)
            and all(record["kind"] != "listener_error" for record in records), "publisher logs invalid")
    return records


def assert_requests(records, wanted):
    require([(record["case"], record["request"], record["status"]) for record in records]
            == [("pair", endpoint, 200) for endpoint in wanted]
            and all(record["expected_client"] and record["approved_peer"] for record in records),
            "unexpected publisher requests")


def chromium_processes(podman, browser):
    probe = """from pathlib import Path
live = zombies = 0
for process in Path('/proc').glob('[0-9]*/status'):
    try:
        lines = process.read_text().splitlines()
        if lines[0].split()[-1] == 'chrome':
            state = next(line.split()[1] for line in lines if line.startswith('State:'))
            zombies += state == 'Z'
            live += state != 'Z'
    except (FileNotFoundError, PermissionError, ProcessLookupError):
        pass
print(live, zombies)
"""
    return tuple(map(int, command([*podman, "exec", browser, "python", "-c", probe], timeout=4).split()))


def start_driver(podman, browser, ca):
    return subprocess.Popen(
        [*podman, "exec", "-i", "--env", "MQ_FIXTURE_TEST_CA=" + ca,
         browser, "python", "-m", "lifecycle.driver_ipc"],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0,
    )


def require_drained_browser(podman, browser):
    wait_until = time.monotonic() + 5
    while any(counts := chromium_processes(podman, browser)):
        if time.monotonic() >= wait_until:
            emit("chromium_cleanup_failure", live=counts[0], zombies=counts[1])
            raise TimeoutError("Chromium remained after session close")
        time.sleep(0.1)


def stop_browser_after_failed_revoke(podman, browser, error):
    # Stop the isolated browser before releasing a transport with unconfirmed eligibility.
    command([*podman, "stop", "--time", "0", browser], timeout=6)
    try:
        if error.driver is not None:
            stop(error.driver)
    finally:
        if error.connection is not None:
            error.connection.close()


def controller_inspect(podman, name, directory):
    record, = json.loads(command([*podman, "inspect", name]))
    networks = record["NetworkSettings"]["Networks"] or {}
    checks = dict(running=record["State"]["Running"],
                  network_mode_none=record["HostConfig"]["NetworkMode"] == "none",
                  no_network_attachments=set(networks) <= {"none"} and not any(
                      settings.get(field) for settings in networks.values()
                      for field in ("IPAddress", "GlobalIPv6Address", "Gateway", "IPv6Gateway")),
                  readonly_root=record["HostConfig"]["ReadonlyRootfs"])
    if not all(checks.values()):
        emit("controller_setup_failure", **checks, exit_code=record["State"]["ExitCode"])
        if not checks["running"]:
            output = entries(command([*podman, "logs", name]))
            emit("controller_setup_log", errors=[entry.get("error_type") for entry in output
                                                  if entry.get("kind") == "controller_error"])
    require(all(checks.values()), "controller process or isolation invalid")
    command([*podman, "exec", name, "python", "-c",
             "import socket; assert {name for _, name in socket.if_nameindex()} == {'lo'}"], timeout=4)
    mount, = record["Mounts"]
    require((mount["Type"], mount["Source"], mount["Destination"], mount["RW"])
            == ("bind", str(directory), "/fixture-control", False), "controller mounted unexpected paths")
    require(not any(entry.split("=", 1)[0].startswith(("AWS_", "MIAOQIA_"))
                    for entry in record["Config"]["Env"]), "controller inherited role settings")
    return int(record["State"]["Pid"])


def kill_at_body(podman, name, pidfd, browser):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        records = entries(command([*podman, "logs", name], timeout=3))
        if any(record.get("kind") == "body_paused" for record in records):
            live, _ = chromium_processes(podman, browser)
            require(live > 0, "no Chromium at body pause")
            signal.pidfd_send_signal(pidfd, signal.SIGKILL)
            return
        time.sleep(0.1)
    raise TimeoutError("controller body pause not observed")


def exercise(root, podman, lease, browser, image, ca, peer, label, mode):
    directory = root / ("ipc-" + mode)
    directory.mkdir(mode=0o700)
    path = directory / "driver.sock"
    name = f"{browser}-control-{mode}"
    uid, gid = os.getuid(), os.getgid()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
        listener.bind(str(path))
        path.chmod(0o600)
        listener.listen(1)
        try:
            command([*podman, "run", "-d", "--name", name, "--label", label,
                     "--pull=never", "--http-proxy=false", "--network=none", "--userns=keep-id",
                     "--user", f"{uid}:{gid}", "--read-only", "--cap-drop=ALL",
                     "--security-opt=no-new-privileges", "--security-opt=label=disable",
                     "--pids-limit=64", "--memory=256m",
                     "--cpus=1", "--log-driver=k8s-file",
                     "--volume", f"{directory}:/fixture-control:ro", image,
                     "python", "-m", "lifecycle.controller_ipc", mode, peer])
            pid = controller_inspect(podman, name, directory)

            pidfd = os.pidfd_open(pid)
            try:
                require(controller_inspect(podman, name, directory) == pid, "controller identity changed")
                with ThreadPoolExecutor(max_workers=1) as executor:
                    killer = executor.submit(kill_at_body, podman, name, pidfd, browser) if mode == "hold" else None
                    try:
                        result = serve(listener, lease, lambda: start_driver(podman, browser, ca), controller_pid=pid,
                                       controller_uid=uid, controller_gid=gid, controller_pidfd=pidfd)
                    except RevocationError as error:
                        stop_browser_after_failed_revoke(podman, browser, error)
                        raise
                    if killer:
                        killer.result()
            finally:
                os.close(pidfd)
            exit_status = int(command([*podman, "wait", name], timeout=5).strip())
            output = entries(command([*podman, "logs", name]))
            if exit_status not in (0, 137):
                emit("controller_failure", mode=mode, exit_status=exit_status,
                     errors=[{key: entry.get(key) for key in ("error_type", "stage", "line")}
                             for entry in output if entry.get("kind") == "controller_error"])
            require(exit_status == (0 if mode == "pair" else 137) and not lease.eligible(),
                    "controller wait or revocation failed")
            require(result["granted"] and result["revoked"]
                    and result["reason"] in {"controller_exit", "ipc_eof"}, "invalid session termination")
            require_drained_browser(podman, browser)
            return result, output, exit_status
        finally:
            command([*podman, "rm", "--force", "--time", "1", "--ignore", name], timeout=8)
            path.unlink(missing_ok=True)


def drill(run, namespace, client, peer, browser, publisher, image, seccomp_sha):
    root = Path(run)
    podman = ["podman", "--root", str(root / "graph"), "--runroot", str(root / "run")]
    client, peer = str(IPv4Address(client)), str(IPv4Address(peer))
    ca = base64.b64encode((root / "keys/ca.crt").read_bytes()).decode("ascii")
    label = f"io.miaoqia.fixture={root.name.lower()}"

    stage = "installed_policy"
    try:
        inputs = ("chromium_home.py", "verify-confinement.sh", "lifecycle/composition_ipc.py",
                  "lifecycle/controller_ipc.py", "lifecycle/driver_ipc.py", "lifecycle/ipc_session.py")
        emit("build", image_id=image, seccomp_sha256=seccomp_sha,
             staged_context_sha256=(root / "context.sha").read_text().strip(),
             input_sha256={name: sha256(Path(name).read_bytes()).hexdigest() for name in inputs})
        lease = EgressLease(podman, namespace, client, peer)
        try:
            require(not lease.eligible(), stage)
            require(not requests(publisher_health_records(podman, publisher, ca)),
                    "publisher received requests before grant")
            stage = "separate_process_pair"
            pair, results, _ = exercise(root, podman, lease, browser, image, ca, peer, label, "pair")
            before = publisher_health_records(podman, publisher, ca)
            positive = requests(before)
            assert_requests(positive, ["view", "data"])
            capture, = (result for result in results if result.get("kind") == "pair")
            tls_names = {"TLSv1.2": "TLS 1.2", "TLSv1.3": "TLS 1.3"}
            require(capture["permit_completions"] == 2
                    and capture["original_sha256"] == positive[-1]["body_sha256"]
                    and capture["same_navigation"] is True and capture["both_cdp_peers_match_pin"] is True
                    and capture["tls"] == [tls_names[entry["tls"]] for entry in positive],
                    "pair receipts diverged")
            emit("pair", **{key: value for key, value in capture.items() if key != "kind"}, session=pair)

            stage = "controller_loss_during_body"
            cut, results, status = exercise(root, podman, lease, browser, image, ca, peer, label, "hold")
            after = publisher_health_records(podman, publisher, ca)
            require(after[:len(before)] == before, "publisher log prefix changed")
            remaining = requests(after[len(before):])
            assert_requests(remaining, ["view"])
            require(sum(result.get("kind") == "body_paused" for result in results) == 1
                    and all(result.get("kind") != "pair" for result in results), "hold returned invalid output")
            emit("controller_loss", body_permit_paused=True, sigkill=status == 137, remaining_body_arrivals=0,
                 browser_processes_removed=True, publisher_health_verified=True, session=cut)
        finally:
            lease.close()
    except Exception as error:
        emit("incomplete", stage=stage, error=type(error).__name__,
             location=[[Path(frame.filename).name, frame.lineno, frame.name]
                       for frame in extract_tb(error.__traceback__)[-5:]])
        return 1
    emit("result", provider_ipc_fixture_passed=True, all_target_http_policy_verified=False)
    return 0


if __name__ == "__main__":
    raise SystemExit(drill(*sys.argv[1:]))
