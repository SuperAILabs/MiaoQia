"""A controller pidfd and a nonrenewing nftables lease for one owned network namespace."""

import json
import os
import select
import signal
import subprocess
import time


LEASE_SECONDS = 20
TABLE = "mq_lifecycle"


def command(args, *, script=None, timeout=10, pass_fds=()):
    return subprocess.run(args, input=script, capture_output=True, text=True, check=True, timeout=timeout,
                          pass_fds=pass_fds).stdout


class EgressLease:
    def __init__(self, podman, namespace, client, peer):
        self.namespace_path = namespace
        self.namespace = os.open(namespace, os.O_RDONLY | os.O_CLOEXEC)
        self.inside = [*podman, "unshare", "nsenter", f"--net={namespace}"]
        self.peer = peer
        try:
            if os.fstat(self.namespace).st_ino == os.stat("/proc/self/ns/net").st_ino:
                raise RuntimeError("expected an independent network namespace")
            for family in ("-4", "-6"):
                if self.run_inside("ip", family, "route", "show", "default").strip():
                    raise RuntimeError("unexpected default route")
            self.nft(f"""table inet {TABLE} {{
  set eligible {{ type ipv4_addr; flags timeout; }}
  counter denied_output {{ }}
  counter denied_syn {{ }}
  chain output {{
    type filter hook output priority -150; policy drop;
    ip saddr {client} ip daddr @eligible tcp dport 443 accept
    ip saddr {client} ip daddr {peer} tcp dport 443 tcp flags & (syn | ack) == syn counter name denied_syn drop
    counter name denied_output drop
  }}
  chain input {{
    type filter hook input priority -150; policy drop;
    ip daddr {client} ip saddr @eligible tcp sport 443 ct state established accept
  }}
}}
""")
        except BaseException:
            self.close()
            raise

    def nft(self, script):
        self.run_inside("nft", "-f", "-", script=script)

    def run_inside(self, *args, script=None):
        identity = os.stat(self.namespace_path)
        held = os.fstat(self.namespace)
        if (identity.st_dev, identity.st_ino) != (held.st_dev, held.st_ino):
            raise RuntimeError("network namespace identity changed")
        return command([*self.inside, *args], script=script, timeout=4)

    def eligible(self):
        rules = json.loads(self.run_inside("nft", "-j", "list", "set", "inet", TABLE, "eligible"))
        lease, = (entry["set"] for entry in rules["nftables"] if "set" in entry)
        return bool(lease.get("elem"))

    def denied_packets(self, *, syn=False):
        name = "denied_syn" if syn else "denied_output"
        rules = json.loads(self.run_inside("nft", "-j", "list", "counter", "inet", TABLE, name))
        counter, = (entry["counter"] for entry in rules["nftables"] if "counter" in entry)
        return counter["packets"]

    def close(self):
        os.close(self.namespace)


class ControllerWatch:
    def __init__(self, lease, controller):
        self.lease = lease
        self.controller = controller
        self.pidfd = os.pidfd_open(controller.pid)
        self.poller = select.poll()
        self.poller.register(self.pidfd, select.POLLIN)

    def grant(self, seconds=LEASE_SECONDS):
        if not 1 <= seconds <= LEASE_SECONDS or self.poller.poll(0) or self.lease.eligible():
            raise RuntimeError("controller or lease is ineligible")
        self.ttl = seconds
        self.granted_at = time.monotonic()
        try:
            self.lease.nft(f"add element inet {TABLE} eligible {{ {self.lease.peer} timeout {self.ttl}s }}\n")
            if self.poller.poll(0) or not self.lease.eligible():
                raise RuntimeError("grant raced controller exit")
        except BaseException:
            self.revoke()
            raise

    def revoke(self):
        self.lease.nft(f"flush set inet {TABLE} eligible\n")
        if self.lease.eligible():
            raise RuntimeError("eligibility remained after revoke")

    def wait_and_revoke(self, *, include_observed_at=False):
        try:
            waiting = time.monotonic_ns()
            if self.poller.poll(0):
                raise RuntimeError("controller exited before watchdog began waiting")
            if not self.poller.poll(3000):
                raise TimeoutError("controller exit was not observed")
            observed = time.monotonic()
            status = self.controller.wait(timeout=2)
        finally:
            self.revoke()
        finished = time.monotonic()
        result = {
            "controller_sigkill": status == -signal.SIGKILL,
            "pidfd_exit_observed": True,
            "observed_to_revoke_ms": round(1000 * (finished - observed)),
            "revoked_before_lease_expiry": finished - self.granted_at < self.ttl,
        }
        if include_observed_at:
            result["observed_at_ns"] = round(observed * 1_000_000_000)
        return waiting, result

    def close(self):
        os.close(self.pidfd)
