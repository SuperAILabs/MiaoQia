"""Serve one authenticated Playwright stdio session from the fixture host."""

import errno
import math
import os
import select
import socket
import stat
import struct
import subprocess
import time

from supervisor import LEASE_SECONDS, TABLE


_HELLO = b"MQPW1\n"
_ACK = b"\x01"
_BUFFER_LIMIT = 64 * 1024
_CREDENTIALS = struct.Struct("@iII")
_PEER_PIDFD = getattr(socket, "SO_PEERPIDFD", 77)
_POLL_ERRORS = select.POLLERR | select.POLLNVAL


class RevocationError(RuntimeError):
    """Preserve the live transport for caller recovery when revoke is unconfirmed."""

    def __init__(self, driver, connection):
        super().__init__("could not confirm empty fixture egress eligibility")
        self.driver = driver
        self.connection = connection


class _SessionEnded(Exception):
    pass


class _Transport:
    def __init__(self, controller_fd, deadline):
        self.deadline = deadline
        self.poll = select.poll()
        self.poll.register(controller_fd, select.POLLIN)
        self.controller_fd = controller_fd
        self.connection = None
        self.driver_fd = self.driver_in = self.driver_out = None

    def events(self, *, wait=True):
        remaining = self.deadline - time.monotonic()
        events = dict(self.poll.poll(math.ceil(max(0, remaining) * 1000) if wait else 0))
        if self.controller_fd in events:
            raise _SessionEnded("controller_exit")
        if self.driver_fd in events:
            raise _SessionEnded("driver_exit")
        if self.connection is not None:
            socket_events = events.get(self.connection.fileno(), 0)
            if socket_events & _POLL_ERRORS:
                raise _SessionEnded("ipc_error")
            if socket_events & (select.POLLHUP | select.POLLRDHUP):
                raise _SessionEnded("ipc_eof")
        for pipe in (self.driver_in, self.driver_out):
            if events.get(pipe, 0) & (_POLL_ERRORS | select.POLLHUP):
                raise _SessionEnded("driver_exit")
        if time.monotonic() >= self.deadline:
            raise _SessionEnded("timeout")
        return events

    def accept(self, listener):
        self.poll.register(listener, select.POLLIN)
        saved_timeout = listener.gettimeout()
        try:
            listener.setblocking(False)
            while True:
                listener_events = self.events().get(listener.fileno(), 0)
                if listener_events & (_POLL_ERRORS | select.POLLHUP):
                    raise RuntimeError("fixture listener failed")
                if not listener_events & select.POLLIN:
                    continue
                try:
                    self.connection, _ = listener.accept()
                    break
                except BlockingIOError:
                    continue
        finally:
            listener.settimeout(saved_timeout)
            self.poll.unregister(listener)
        self.connection.setblocking(False)
        self.poll.register(self.connection, select.POLLRDHUP)

    def authenticate(self, expected):
        credentials = self.connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, _CREDENTIALS.size)
        if _CREDENTIALS.unpack(credentials) != expected:
            raise _SessionEnded("unauthorized_peer")
        try:
            peer_fd = self.connection.getsockopt(socket.SOL_SOCKET, _PEER_PIDFD)
        except ProcessLookupError:
            self.events(wait=False)
            raise _SessionEnded("unauthorized_peer") from None
        except OSError as error:
            if error.errno == errno.ENOPROTOOPT:
                raise RuntimeError("fixture host lacks SO_PEERPIDFD") from error
            raise
        try:
            self.poll.register(peer_fd, select.POLLIN)
            if peer_fd in self.events(wait=False):
                raise _SessionEnded("unauthorized_peer")
        finally:
            self.poll.unregister(peer_fd)
            os.close(peer_fd)

        received = bytearray()
        self.poll.modify(self.connection, select.POLLRDHUP | select.POLLIN)
        while len(received) < len(_HELLO):
            self.events()
            received.extend(_read(self.connection.recv, len(_HELLO) - len(received), "ipc"))
            if not _HELLO.startswith(received):
                raise _SessionEnded("bad_hello")
        self.poll.modify(self.connection, select.POLLRDHUP)

    def attach_driver(self, driver):
        if driver.stdin is None or driver.stdout is None or driver.stderr is not None:
            raise ValueError("driver needs stdin and stdout pipes and external stderr handling")
        self.driver_in = driver.stdin.fileno()
        self.driver_out = driver.stdout.fileno()
        for pipe in (self.driver_in, self.driver_out):
            if not stat.S_ISFIFO(os.fstat(pipe).st_mode):
                raise ValueError("driver stdin and stdout must be pipes")
            os.set_blocking(pipe, False)
            self.poll.register(pipe, 0)
        self.driver_fd = os.pidfd_open(driver.pid)
        self.poll.register(self.driver_fd, select.POLLIN)

    def relay(self):
        self.poll.modify(self.connection, select.POLLRDHUP | select.POLLOUT)
        while True:
            self.events()
            if _write(self.connection.send, _ACK, "ipc"):
                break

        to_driver = bytearray()
        to_controller = bytearray()
        while True:
            interests = select.POLLRDHUP
            if len(to_driver) < _BUFFER_LIMIT:
                interests |= select.POLLIN
            if to_controller:
                interests |= select.POLLOUT
            self.poll.modify(self.connection, interests)
            self.poll.modify(self.driver_in, select.POLLOUT if to_driver else 0)
            self.poll.modify(self.driver_out, select.POLLIN if len(to_controller) < _BUFFER_LIMIT else 0)
            events = self.events()
            socket_events = events.get(self.connection.fileno(), 0)
            if socket_events & select.POLLIN:
                to_driver.extend(_read(self.connection.recv, _BUFFER_LIMIT - len(to_driver), "ipc"))
            if socket_events & select.POLLOUT:
                del to_controller[:_write(self.connection.send, to_controller, "ipc")]
            if events.get(self.driver_out, 0) & select.POLLIN:
                to_controller.extend(_read(self.read_driver, _BUFFER_LIMIT - len(to_controller), "driver"))
            if events.get(self.driver_in, 0) & select.POLLOUT:
                del to_driver[:_write(self.write_driver, to_driver, "driver")]

    def read_driver(self, size):
        return os.read(self.driver_out, size)

    def write_driver(self, data):
        return os.write(self.driver_in, data)


def _read(read, size, source):
    try:
        data = read(size)
    except BlockingIOError:
        return b""
    except OSError as error:
        raise _SessionEnded(f"{source}_error") from error
    if not data:
        raise _SessionEnded("ipc_eof" if source == "ipc" else "driver_exit")
    return data


def _write(write, data, destination):
    try:
        size = write(data)
    except BlockingIOError:
        return 0
    except OSError as error:
        raise _SessionEnded(f"{destination}_error") from error
    if not size:
        raise _SessionEnded(f"{destination}_error")
    return size


def _revoke(lease):
    try:
        lease.nft(f"flush set inet {TABLE} eligible\n")
    except Exception:
        if lease.eligible():
            raise
        return
    if lease.eligible():
        raise RuntimeError("eligibility remained after revoke")


def _close_driver(driver):
    try:
        for stream in (driver.stdin, driver.stdout, driver.stderr):
            if stream is not None:
                stream.close()
    finally:
        _reap_driver(driver)


def _reap_driver(driver):
    for stop in (driver.terminate, driver.kill):
        try:
            driver.wait(timeout=1)
            return
        except subprocess.TimeoutExpired:
            stop()
    driver.wait(timeout=1)


def _check_pinned_controller(transport, controller_pid):
    with open(f"/proc/self/fdinfo/{transport.controller_fd}", encoding="ascii") as info:
        observed = next((line.split(":", 1)[1].strip() for line in info if line.startswith("Pid:")), None)
    if observed is None:
        raise ValueError("controller_pidfd is not a pidfd")
    transport.events(wait=False)
    if observed != str(controller_pid):
        raise ValueError("controller_pidfd does not refer to controller_pid")


def serve(listener, lease, driver_factory, *, controller_pid, controller_uid, controller_gid,
          timeout=20, controller_pidfd=None):
    """Bridge one peer on a private, exclusively served, caller-owned Unix listener.

    The host kernel must support SO_PEERCRED, SO_PEERPIDFD and pidfd_open.
    The caller supplies a live inspected controller and an exclusive empty lease.
    The controller's mapped real and effective UID/GID must match at connect.
    An optional caller-owned controller_pidfd pins identity across setup and is
    verified using procfs mounted for the host PID namespace.
    The trusted factory promptly starts one owned fixed command (binary PIPEs,
    stderr inherited or redirected) and returns its Popen without waiting for I/O.
    The monotonic session timeout covers accept through relay. A running lease
    operation can finish after that deadline. Revoke uses the lease's bounded
    flush and inspection calls, then driver close/reap allows up to three seconds.
    Expected losses return reason, granted and revoked. Infrastructure failures
    raise. On RevocationError the caller retains the live driver and connection
    via the exception and must isolate the fixture before closing them.
    """
    if not math.isfinite(timeout) or not 0 < timeout <= LEASE_SECONDS:
        raise ValueError(f"fixture timeout must be positive and at most {LEASE_SECONDS}s")
    if listener.family != socket.AF_UNIX or listener.type != socket.SOCK_STREAM:
        raise ValueError("fixture listener must be a Unix stream socket")
    receipt = {"reason": None, "granted": False, "revoked": False}
    deadline = time.monotonic() + timeout
    try:
        controller_fd = os.pidfd_open(controller_pid) if controller_pidfd is None else os.dup(controller_pidfd)
    except ProcessLookupError:
        receipt["reason"] = "controller_exit"
        return receipt

    transport = _Transport(controller_fd, deadline)
    driver = None
    grant_attempted = False
    try:
        if controller_pidfd is not None:
            _check_pinned_controller(transport, controller_pid)
        transport.accept(listener)
        transport.authenticate((controller_pid, controller_uid, controller_gid))
        if lease.eligible():
            raise RuntimeError("fixture egress lease is already eligible")
        transport.events(wait=False)
        driver = driver_factory()
        transport.attach_driver(driver)
        transport.events(wait=False)
        ttl = min(LEASE_SECONDS, max(1, math.ceil(deadline - time.monotonic())))
        grant_attempted = True
        lease.nft(f"add element inet {TABLE} eligible {{ {lease.peer} timeout {ttl}s }}\n")
        receipt["granted"] = True
        transport.events(wait=False)
        if not lease.eligible():
            raise RuntimeError("fixture egress grant was not confirmed")
        transport.relay()
    except _SessionEnded as ended:
        receipt["reason"] = ended.args[0]
    finally:
        try:
            if grant_attempted:
                try:
                    _revoke(lease)
                except BaseException as error:
                    raise RevocationError(driver, transport.connection) from error
                receipt["revoked"] = True
            try:
                if driver is not None:
                    _close_driver(driver)
            finally:
                if transport.connection is not None:
                    transport.connection.close()
        finally:
            if transport.driver_fd is not None:
                os.close(transport.driver_fd)
            os.close(controller_fd)
    return receipt
