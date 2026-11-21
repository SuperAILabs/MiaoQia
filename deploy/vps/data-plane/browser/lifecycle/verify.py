"""Exercise one controller lease using a private rootless Podman store and bridge."""

from contextlib import ExitStack
import fcntl
from hashlib import sha256
import ipaddress
import json
import os
from pathlib import Path
import re
import select
import shutil
import subprocess
import sys
import time

from probe import emit
from supervisor import ControllerWatch, EgressLease, LEASE_SECONDS, command


IMAGE = "mcr.microsoft.com/playwright/python@sha256:abf13b369f8829eb45e29df38d6c5221f7e7521649cb5d2de7989c82bdb574ad"
SOURCE = Path(__file__).resolve().parent


def require(condition, stage):
    if not condition:
        raise RuntimeError(stage)


def stop(process):
    if process.poll() is None:
        process.kill()
    process.wait(timeout=5)


def read_ready(process):
    if not select.select([process.stdout], [], [], 5)[0]:
        raise TimeoutError("client response deadline")
    return json.loads(process.stdout.readline())


class Drill:
    def __init__(self, run_id):
        require(re.fullmatch(r"[a-f0-9]{16}", run_id), "invalid run id")
        self.name = f"mq-browser-lifecycle-{run_id}"
        self.root = Path("/var/tmp") / self.name
        self.podman = ["podman", "--root", str(self.root / "graph"), "--runroot", str(self.root / "runtime")]
        self.label = f"io.miaoqia.lifecycle={self.name}"
        self.client = f"{self.name}-client"
        self.peer = f"{self.name}-peer"
        self.namespace_file = self.root / "namespace"
        self.stage = "platform_preflight"

    def pod(self, *args, **kwargs):
        return command([*self.podman, *args], **kwargs).strip()

    def run(self):
        require(os.geteuid() != 0 and self.pod("info", "--format", "{{.Host.Security.Rootless}}") == "true", self.stage)
        self.stage = "image_pull"
        self.pod("pull", "--quiet", IMAGE, timeout=150)
        image_id = self.pod("image", "inspect", "--format", "{{.Id}}", IMAGE)
        hashes = {name: sha256((SOURCE / name).read_bytes()).hexdigest()
                  for name in ("probe.py", "supervisor.py", "verify.py", "verify-lifecycle.sh")}
        emit("build", image_digest=IMAGE.split("@", 1)[1], image_id=image_id, input_sha256=hashes,
             podman=command(["podman", "version", "--format", "{{.Client.Version}}"]).strip(),
             lease_seconds=LEASE_SECONDS)
        self.stage = "network_setup"
        self.pod("network", "create", "--internal", "--disable-dns", "--label", self.label, self.name)
        network, = json.loads(self.pod("network", "inspect", self.name))
        require(network["internal"] and not network["dns_enabled"] and not network["ipv6_enabled"], self.stage)
        subnet = ipaddress.IPv4Network(network["subnets"][0]["subnet"])
        client_address, peer_address = map(str, (subnet[51], subnet[50]))
        common = ["run", "-d", "--pull=never", "--http-proxy=false", "--label", self.label,
                  "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                  "--network", self.name, "--pids-limit", "32", "--memory", "128m", "--cpus", "1",
                  "--user", "1001:1001", "--log-driver", "k8s-file"]
        probe = (SOURCE / "probe.py").read_text()
        self.pod(*common, "--ip", peer_address, "--name", self.peer, "--sysctl", "net.ipv4.ip_unprivileged_port_start=443",
                 IMAGE, "timeout", "90", "python", "-u", "-c", probe, "peer", peer_address, client_address)
        for _ in range(12):
            if self.pod("logs", self.peer) == '{"kind": "ready"}':
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("peer did not become ready")
        client_id = self.pod(*common, "--ip", client_address, "--name", self.client, IMAGE, "sleep", "90")
        inspect, = json.loads(self.pod("inspect", client_id))
        require(set(inspect["NetworkSettings"]["Networks"]) == {self.name} and not inspect["Mounts"], self.stage)
        namespace = inspect["NetworkSettings"]["SandboxKey"]
        require(bool(namespace), self.stage)
        self.namespace_file.write_text(namespace)
        self.stage = "policy_install"
        with ExitStack() as stack:
            lease = EgressLease(self.podman, namespace, client_address, peer_address)
            stack.callback(lease.close)
            require(not lease.eligible(), self.stage)
            controller = subprocess.Popen(["sleep", "45"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                          stderr=subprocess.DEVNULL)
            stack.callback(stop, controller)
            watch = ControllerWatch(lease, controller)
            stack.callback(watch.close)
            worker = subprocess.Popen(
                [*self.podman, "exec", "-i", client_id, "python", "-u", "-c", probe, "client", peer_address],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
            )
            stack.callback(stop, worker)
            require(read_ready(worker) == {"kind": "ready"}, self.stage)
            self.stage = "pregrant"
            before = lease.denied_packets(syn=True)
            require(self.probe(worker, "new").get("timed_out") is True and lease.denied_packets(syn=True) > before, self.stage)
            self.stage = "lease_expiry"
            stack.callback(watch.revoke)
            watch.grant(seconds=4)
            require(self.probe(worker, "live_new") == {"kind": "live_new", "connected": True}, self.stage)
            time.sleep(4)
            before_expired_probe = lease.denied_packets(syn=True)
            require(self.probe(worker, "new") == {"kind": "new", "timed_out": True} and not lease.eligible(), self.stage)
            expired_probe_drops = lease.denied_packets(syn=True) - before_expired_probe
            require(expired_probe_drops > 0 and controller.poll() is None, self.stage)
            emit("expiry", lease_seconds=4, controller_alive=True, expired_without_revoke=True,
                 new_socket_drop_packets=expired_probe_drops)
            watch.grant()
            self.stage = "live_controller"
            for phase in ("before", "again"):
                require(self.probe(worker, phase) == {"kind": phase, "acknowledged": True, "established": True}, self.stage)
            require(self.probe(worker, "live_new") == {"kind": "live_new", "connected": True}, self.stage)
            require(controller.poll() is None, self.stage)
            self.stage = "controller_exit"
            injector = subprocess.Popen(
                [sys.executable, "-c", "import signal,sys,time; time.sleep(0.5); instant=time.monotonic_ns(); "
                 "signal.pidfd_send_signal(int(sys.argv[1]), signal.SIGKILL); print(instant)", str(watch.pidfd)],
                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                text=True, pass_fds=(watch.pidfd,),
            )
            stack.callback(stop, injector)
            waiting, loss = watch.wait_and_revoke()
            injection_timestamp, _ = injector.communicate(timeout=2)
            loss["watchdog_wait_started_before_kill"] = injector.returncode == 0 and waiting < int(injection_timestamp)
            loss["selector_scope"] = "supervisor_owned_child"
            emit("controller", **loss)
            require(all(loss[key] for key in ("controller_sigkill", "pidfd_exit_observed", "revoked_before_lease_expiry",
                                             "watchdog_wait_started_before_kill")), self.stage)
            self.stage = "post_revoke"
            before = lease.denied_packets()
            established = self.probe(worker, "after")
            midpoint = lease.denied_packets()
            syn_before = lease.denied_packets(syn=True)
            fresh = self.probe(worker, "new")
            syn_after = lease.denied_packets(syn=True)
            require(established == {"kind": "after", "established": True, "timed_out": True} and midpoint > before, self.stage)
            require(fresh == {"kind": "new", "timed_out": True} and syn_after > syn_before, self.stage)
            for owned in (client_id, self.peer):
                require(self.pod("inspect", "--format", "{{.State.Running}}", owned) == "true", self.stage)
            self.stage = "peer_evidence"
            records = [json.loads(line) for line in self.pod("logs", self.peer).splitlines()]
            hit = {"kind": "connection", "expected_peer": True, "expected_client": True}
            require(records == [{"kind": "ready"}, hit, hit, {"kind": "challenge", "phase": "before"},
                                {"kind": "challenge", "phase": "again"}, hit], self.stage)
            emit("egress", rootless=True, no_default_route=True, pregrant_blocked=True,
                 containers_alive_during_probes=True, persistent_tcp_established_after_revoke=True,
                 existing_socket_drop_packets=midpoint - before, new_socket_drop_packets=syn_after - syn_before,
                 accepted_connections_before_kill=2, accepted_connections_expiry_control=1,
                 acknowledged_before=2, accepted_after=0, challenges_after=0)
        self.stage = "supervisor_reap"
        require(self.pod("rm", "--force", "--time", "2", client_id) == client_id, self.stage)
        require(not Path(namespace).exists(), self.stage)
        emit("supervisor", owned_client_removed=True, namespace_unlinked=True)

    def probe(self, worker, phase):
        worker.stdin.write(phase + "\n")
        worker.stdin.flush()
        return read_ready(worker)

    def cleanup(self):
        if not (self.root / "graph").exists():
            return
        namespace = self.namespace_file.read_text() if self.namespace_file.is_file() else None
        containers = self.pod("ps", "-aq", "--filter", f"label={self.label}").splitlines()
        if containers:
            self.pod("rm", "--force", "--time", "2", *containers, timeout=18)
        networks = self.pod("network", "ls", "-q", "--filter", f"label={self.label}").splitlines()
        if networks:
            self.pod("network", "rm", *networks, timeout=15)
        require(not self.pod("ps", "-aq", "--filter", f"label={self.label}"), "containers remain")
        require(not self.pod("network", "ls", "-q", "--filter", f"label={self.label}"), "network remains")
        require(namespace is None or not Path(namespace).exists(), "namespace remains")

    def execute(self, recovery):
        if recovery and not self.root.exists() and not self.root.is_symlink():
            emit("cleanup", verified=True, already_absent=True)
            return 0
        require(not self.root.is_symlink() and self.root.stat().st_uid == os.geteuid(), "invalid run owner")
        require((self.root / "owner").read_text().strip() == self.name, "invalid run marker")
        with (self.root / "lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            passed = False
            try:
                if not recovery:
                    self.run()
                passed = True
            except Exception as error:
                emit("incomplete", stage=self.stage, error=type(error).__name__)
            finally:
                self.cleanup()
                stores = [self.root / name for name in ("graph", "runtime") if (self.root / name).exists()]
                if stores:
                    command(["podman", "unshare", "rm", "-rf", "--", *stores], timeout=25)
                shutil.rmtree(self.root)
                require(not self.root.exists(), "run storage remains")
                emit("cleanup", verified=True, containers=0, networks=0, storage_removed=True)
        return 0 if passed else 1


if __name__ == "__main__":
    try:
        raise SystemExit(Drill(sys.argv[2]).execute(sys.argv[1] == "cleanup"))
    except Exception as error:
        emit("incomplete", stage="ownership_or_cleanup", error=type(error).__name__)
        raise SystemExit(1) from None
