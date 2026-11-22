"""Run and verify the real acquisition browser role in the disposable fixture."""

import base64
from hashlib import sha256
from ipaddress import IPv4Address, IPv4Network
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import time
from traceback import extract_tb

from composition_ipc import (entries, publisher_health_records, requests, require_drained_browser, start_driver,
                             stop_browser_after_failed_revoke)
from ipc_session import RevocationError, serve
from probe import emit
from supervisor import EgressLease, command
from verify import require


POSTGRES = (
    "docker.io/library/postgres@sha256:b86568d3e0fe1dfaeff52714f9da36f206a30e4c49131b82bf96982d78627409"
)
SEAWEED = (
    "docker.io/chrislusf/seaweedfs@sha256:e7d4b80af9bd49b2630a60ac6fbb7ffb620ac6a2c501f689c22a71b9f89f0203"
)


def write_environment(path, values):
    with path.open("x", encoding="ascii") as stream:
        os.chmod(path, 0o600)
        for key, value in values.items():
            stream.write(f"{key}={value}\n")


def backing_services(root, podman, prefix, label, network, peer):
    """Start replacements with fresh storage and no published ports."""
    record, = json.loads(command([*podman, "network", "inspect", network]))
    require(record["internal"] and not record["dns_enabled"] and not record["ipv6_enabled"],
            "fixture backing network isolation")
    subnet = IPv4Network(record["subnets"][0]["subnet"])
    pg, seaweed = str(subnet[51]), str(subnet[52])
    for image in (POSTGRES, SEAWEED):
        command([*podman, "pull", "--quiet", image], timeout=175)
    admin_secret, owner_secret, app_secret = (secrets.token_hex(24) for _ in range(3))
    write_environment(root / "postgres.env", {"POSTGRES_PASSWORD": admin_secret})
    common = [*podman, "run", "-d", "--pull=never", "--http-proxy=false", "--network", network,
              "--label", label, "--memory", "768m", "--cpus", "1", "--log-driver", "k8s-file"]
    command([*common, "--name", prefix + "-pg", "--ip", pg, "--env-file", str(root / "postgres.env"),
             "--tmpfs", "/var/lib/postgresql/data:rw,mode=0700", POSTGRES,
             "-c", "max_connections=30", "-c", "shared_buffers=48MB"])
    command([*common, "--name", prefix + "-s3", "--ip", seaweed, "--tmpfs", "/data:rw",
             SEAWEED, "server", "-s3", "-dir=/data", "-ip=" + seaweed, "-ip.bind=0.0.0.0",
             "-volume.max=8", "-master.volumeSizeLimitMB=32", "-s3.port=8333"])
    deadline = time.monotonic() + 35
    while True:
        try:
            command([*podman, "exec", prefix + "-pg", "pg_isready", "-q", "-h", "127.0.0.1", "-U", "postgres"],
                    timeout=3)
            break
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
            if time.monotonic() >= deadline:
                raise
        time.sleep(0.25)
    require(not command([*podman, "port", "--all"]).strip(), "fixture published a host port")
    shared = {
        "AWS_ENDPOINT_URL_S3": f"http://{seaweed}:8333",
        "AWS_ACCESS_KEY_ID": secrets.token_hex(12),
        "AWS_SECRET_ACCESS_KEY": secrets.token_hex(24),
        "AWS_DEFAULT_REGION": "us-east-1",
        "AWS_EC2_METADATA_DISABLED": "true",
        "AWS_CONFIG_FILE": "/dev/null",
        "AWS_SHARED_CREDENTIALS_FILE": "/dev/null",
        "MIAOQIA_DEPLOY_DIR": "/fixture/deploy",
        "MIAOQIA_PACKS_DIR": "/fixture-state/packs",
        "MIAOQIA_ACQUISITION_OBJECT_STORE_DIR": "s3://acq-browser-fixture/role",
        "MIAOQIA_ACQUISITION_ARCHIVE_SPOOL_DIR": "/fixture-state/spool",
        "MIAOQIA_ACQUISITION_DB_DSN": f"postgresql://acq_app:{app_secret}@{pg}:5432/acq",
        "MQ_FIXTURE_PUBLISHED_ON": (root / "published-on").read_text().strip(),
        "MQ_FIXTURE_PUBLISHER_IP": peer,
    }
    write_environment(root / "collector.env", {
        **shared, "MIAOQIA_ACQUISITION_BROWSER_RENDERER": "lifecycle.role_provider:make_renderer",
    })
    write_environment(root / "archive-init.env", {
        **shared,
        "MQ_FIXTURE_PG_ADMIN_DSN": f"postgresql://postgres:{admin_secret}@{pg}:5432/postgres",
        "MIAOQIA_ACQUISITION_DB_OWNER_DSN": f"postgresql://acq_owner:{owner_secret}@{pg}:5432/acq",
    })


def role_pid(podman, name, network, state, socket_dir):
    inspect, = json.loads(command([*podman, "inspect", name]))
    require(inspect["State"]["Running"] and inspect["HostConfig"]["ReadonlyRootfs"]
            and set(inspect["NetworkSettings"]["Networks"]) == {network}, "role network or process")
    require({(item["Source"], item["Destination"], item["RW"]) for item in inspect["Mounts"]}
            == {(str(state), "/fixture-state", True), (str(socket_dir), "/fixture-control", False)},
            "role has unexpected mounts")
    require(not any(line.startswith(("MQ_FIXTURE_PG_ADMIN_DSN=", "MIAOQIA_ACQUISITION_DB_OWNER_DSN="))
                    for line in inspect["Config"]["Env"]), "role inherited admin credentials")
    command([*podman, "exec", name, "python", "-c",
             "from pathlib import Path;"
             "assert not any(line.split()[1] == '00000000' for line "
             "in Path('/proc/net/route').read_text().splitlines()[1:])"], timeout=4)
    return int(inspect["State"]["Pid"])


def archive_command(common, root, image, prefix, mode):
    env_file = root / ("archive-init.env" if mode == "init" else "collector.env")
    try:
        output = command([*common, "--rm", "--name", f"{prefix}-{mode}", "--env-file", str(env_file),
                          image, "python", "-m", "lifecycle.role_archive", mode], timeout=65)
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        # This reports only the exception type and source location. Container
        # stderr may include isolated DSNs and stays outside the receipt.
        failure = {"mode": mode, "error": type(error).__name__}
        if isinstance(error.stdout, str):
            try:
                details, = entries(error.stdout)
                if details.get("kind") == "role_archive_error":
                    failure.update({key: details[key] for key in ("stage", "error_type")})
            except (ValueError, KeyError, RuntimeError):
                pass
        emit("archive_error", **failure)
        raise
    return entries(output)


def supervise_sessions(podman, name, network, state, directory, listener, lease, browser, ca):
    pid = role_pid(podman, name, network, state, directory)
    pidfd = os.pidfd_open(pid)
    try:
        require(role_pid(podman, name, network, state, directory) == pid, "role identity changed")
        results = []
        for _ in range(2):
            try:
                result = serve(listener, lease, lambda: start_driver(podman, browser, ca),
                               controller_pid=pid, controller_uid=os.getuid(), controller_gid=os.getgid(),
                               controller_pidfd=pidfd)
            except RevocationError as error:
                stop_browser_after_failed_revoke(podman, browser, error)
                raise
            require(result == {"reason": "ipc_eof", "granted": True, "revoked": True},
                    "each fixture plan must end its own lease")
            require_drained_browser(podman, browser)
            results.append(result)
        return results
    finally:
        os.close(pidfd)


def drill(run, namespace, client, peer, browser, publisher, image, network, seccomp_sha, option=""):
    require(option in ("", "--ingest"), "unknown role fixture option")
    root = Path(run)
    podman = ["podman", "--root", str(root / "graph"), "--runroot", str(root / "run")]
    client, peer = str(IPv4Address(client)), str(IPv4Address(peer))
    ca = base64.b64encode((root / "keys/ca.crt").read_bytes()).decode("ascii")
    prefix = root.name.lower()
    label = f"io.miaoqia.fixture={prefix}"
    name = prefix + "-acq-role"
    stage = "backing_services"
    try:
        inputs = ("Containerfile", "verify-confinement.sh", "lifecycle/composition_role.py",
                  "lifecycle/role_provider.py", "lifecycle/role_archive.py", "lifecycle/ipc_session.py",
                  "deploy/acquisition/roles.yaml", "miaoqia/runtime_acquisition/__init__.py")
        build_details = {}
        if option == "--ingest":
            inputs += ("lifecycle/role_events.py", "lifecycle/role_evidence.py", "deploy/data/roles.yaml",
                       "deploy/platform/roles.yaml", "deploy/platform/platform", "deploy/platform/go-locks.sha256",
                       "miaoqia/runtime_data/__init__.py")
            build_details["go_toolchain"] = Path("deploy/platform/go-version").read_text().strip()
        emit("build", image_id=image, seccomp_sha256=seccomp_sha,
             staged_context_sha256=(root / "context.sha").read_text().strip(),
             input_sha256={path: sha256(Path(path).read_bytes()).hexdigest() for path in inputs},
             postgres_manifest=POSTGRES.split("@", 1)[1], seaweed_manifest=SEAWEED.split("@", 1)[1],
             **build_details)
        backing_services(root, podman, prefix, label, network, peer)
        state, directory = root / "fixture-state", root / "role-control"
        state.mkdir(mode=0o700)
        (state / "spool").mkdir(mode=0o700)
        directory.mkdir(mode=0o700)
        common = [*podman, "run", "--pull=never", "--http-proxy=false", "--network", network,
                  "--label", label, "--userns=keep-id", "--user", f"{os.getuid()}:{os.getgid()}",
                  "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
                  "--pids-limit=96", "--memory=512m", "--cpus=1", "--log-driver=k8s-file"]
        state_mount = ["--volume", f"{state}:/fixture-state:rw"]
        stage = "initialize_archives"
        initialized, = archive_command(
            [*common, *state_mount, "--security-opt=label=disable"], root, image, prefix, "init")
        require(initialized["kind"] == "role_archive_init" and initialized["plan_count"] == 1,
                "expected one materialized browser plan")
        emit("initialization", record=initialized)
        stage = "authenticated_acquisition"
        lease = EgressLease(podman, namespace, client, peer)
        try:
            require(not lease.eligible() and not requests(publisher_health_records(podman, publisher, ca)),
                    "unexpected pregrant traffic")
            path = directory / "driver.sock"
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
                listener.bind(str(path))
                path.chmod(0o600)
                listener.listen(1)
                command([*common, *state_mount, "--security-opt=label=disable", "-d", "--name", name,
                         "--volume", f"{directory}:/fixture-control:ro",
                         "--env-file", str(root / "collector.env"), image, "mq-acquisition", "--role", "acq-browser"])
                sessions = supervise_sessions(podman, name, network, state, directory, listener, lease, browser, ca)
            require(not lease.eligible(), "lease remained active")
            emit("role", command="mq-acquisition --role acq-browser", sessions=sessions,
                 backend_network_private=True, host_ports_published=0)

            stage = "independent_archive_readback"
            verified, = archive_command(
                [*common, "--volume", f"{state}:/fixture-state:ro", "--security-opt=label=disable"],
                root, image, prefix, "verify")
            hits = requests(publisher_health_records(podman, publisher, ca))
            require([(hit["case"], hit["request"], hit["status"]) for hit in hits] == [
                ("role-list", "view", 200), ("role-list", "data", 200),
                ("role-detail", "view", 200), ("role-detail", "data", 200),
            ] and all(hit["expected_client"] and hit["approved_peer"] for hit in hits),
                "publisher did not observe two scoped request pairs")
            require(verified["kind"] == "role_archive_verified"
                    and verified["plan_id"] == initialized["plan_id"]
                    and verified["outbox_count"] == verified["observation_version"] == 1
                    and verified["list_retention_days"] == 30
                    and verified["list_sha256"] == initialized["list_sha256"] == hits[1]["body_sha256"]
                    and verified["original_sha256"] == initialized["original_sha256"] == hits[3]["body_sha256"],
                    "archive and independent publisher receipts diverged")
            emit("archive", record=verified, publisher_requests=4,
                 publisher_list_sha256=hits[1]["body_sha256"], publisher_raw_sha256=hits[3]["body_sha256"])
            stage = "graceful_role_stop"
            command([*podman, "kill", "--signal", "TERM", name], timeout=4)
            require(int(command([*podman, "wait", name], timeout=12)) == 0, "role did not stop cleanly")
            emit("role_stop", exit=0, signal="SIGTERM")
        finally:
            lease.close()
        if option == "--ingest":
            stage = "stop_capture_endpoints"
            command([*podman, "stop", "--time", "2", browser, publisher], timeout=10)
            emit("capture_endpoints_stopped", chromium=True, publisher=True)
            stage = "broker_delivery_and_replay"
            from role_events import run_ingest

            run_ingest(root, podman, prefix, label, network, image, common, state, verified)
    except Exception as error:
        emit("incomplete", stage=stage, error=type(error).__name__,
             location=[[Path(frame.filename).name, frame.lineno, frame.name]
                       for frame in extract_tb(error.__traceback__)[-5:]])
        return 1
    result = {"provider_role_fixture_passed": True, "all_target_http_policy_verified": False}
    if option == "--ingest":
        result["provider_ingest_fixture_passed"] = True
    emit("result", **result)
    return 0


if __name__ == "__main__":
    raise SystemExit(drill(*sys.argv[1:]))
