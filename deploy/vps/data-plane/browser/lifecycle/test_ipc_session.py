import array
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
import errno
import os
import signal
import socket
import struct
import subprocess
import sys
from threading import Event
import time
from uuid import uuid4

import pytest

if sys.platform == "linux":
    import fcntl
    import termios

    from ipc_session import RevocationError, serve


pytestmark = pytest.mark.skipif(sys.platform != "linux", reason="requires Linux Unix peer credentials and pidfds")

HELLO = b"MQPW1\n"
ACK = b"\x01"
WAIT = 5

CONTROLLER = """
import socket
import sys

with socket.socket(fileno=int(sys.argv[1])) as handoff:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.connect(bytes.fromhex(sys.argv[2]))
        socket.send_fds(handoff, [b'!'], [connection.fileno()])

sys.stdin.buffer.read()
"""

ECHO_DRIVER = """
import os

while data := os.read(0, 4096):
    os.write(1, data)
"""

PAUSED_DRIVER = "import signal\nsignal.pause()"


class ObservedSocket(socket.socket):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.accepted = None
        self.received = Event()

    def accept(self):
        connection, address = super().accept()
        self.accepted = ObservedSocket(fileno=connection.detach())
        self.accepted.received = self.received
        return self.accepted, address

    def recv(self, size, flags=0):
        data = super().recv(size, flags)
        if data:
            self.received.set()
        return data


class Lease:
    peer = "192.0.2.2"

    def __init__(self):
        self.active = False
        self.mutations = []
        self.grant_error = self.flush_error = None
        self.after_add = None
        self.before_flush = None

    def eligible(self):
        return self.active

    def nft(self, script):
        words = script.split()
        if words[:2] == ["add", "element"]:
            assert not self.active and self.peer in words
            self.mutations.append("add")
            self.active = True
            if self.after_add is not None:
                self.after_add()
            if self.grant_error is not None:
                raise self.grant_error
        else:
            assert words[:2] == ["flush", "set"] and words[-1] == "eligible", script
            self.mutations.append("flush")
            if self.before_flush is not None:
                self.before_flush()
            if self.flush_error is not None:
                raise self.flush_error
            self.active = False


def stop_process(process):
    if process.poll() is None:
        process.kill()
    process.wait(timeout=WAIT)
    for stream in (process.stdin, process.stdout):
        if stream is not None:
            stream.close()


class Session:
    def __init__(self, stack, listener, executor):
        self.stack = stack
        self.listener = listener
        self.executor = executor
        self.lease = Lease()
        self.drivers = []

    def controller(self):
        parent, child = socket.socketpair()
        with parent, child:
            parent.settimeout(WAIT)
            controller = self.spawn(CONTROLLER, str(child.fileno()), self.listener.getsockname().hex(),
                                    pass_fds=(child.fileno(),))
            message, descriptors, _, _ = socket.recv_fds(parent, 1, 1)
            assert message == b"!" and len(descriptors) == 1
        descriptor, = descriptors
        connection = self.stack.enter_context(socket.socket(fileno=descriptor))
        connection.set_inheritable(False)
        connection.settimeout(WAIT)
        return controller, connection

    def spawn(self, code, *args, **kwargs):
        process = subprocess.Popen([sys.executable, "-u", "-c", code, *args],
                                   stdin=subprocess.PIPE, bufsize=0, **kwargs)
        self.stack.callback(stop_process, process)
        return process

    def pidfd(self, pid):
        descriptor = os.pidfd_open(pid)
        self.stack.callback(os.close, descriptor)
        return descriptor

    def start(self, controller, *, code=ECHO_DRIVER, timeout=WAIT, **identity):
        def driver_factory():
            process = self.spawn(code, stdout=subprocess.PIPE)
            self.drivers.append(process)
            return process

        expected = {"controller_pid": controller.pid, "controller_uid": os.geteuid(),
                    "controller_gid": os.getegid()}
        expected.update(identity)
        return self.executor.submit(serve, self.listener, self.lease, driver_factory, timeout=timeout, **expected)

    def assert_receipt(self, result, reason, *, granted):
        assert result.result(timeout=WAIT + 3) == {"reason": reason, "granted": granted, "revoked": granted}
        assert self.lease.mutations == (["add", "flush"] if granted else [])
        assert not self.lease.eligible()
        assert len(self.drivers) == int(granted)
        if self.listener.accepted is not None:
            assert self.listener.accepted.fileno() == -1
        if granted:
            driver, = self.drivers
            assert driver.poll() is not None
            assert driver.stdin.closed and driver.stdout.closed


@pytest.fixture
def session():
    with ThreadPoolExecutor(max_workers=1) as executor, ExitStack() as stack:
        listener = stack.enter_context(ObservedSocket(socket.AF_UNIX, socket.SOCK_STREAM))
        listener.bind(b"\0mq-ipc-test-" + uuid4().hex.encode())
        listener.listen(1)
        try:
            yield Session(stack, listener, executor)
        finally:
            if listener.accepted is not None:
                listener.accepted.close()


def receive(connection, size):
    received = bytearray()
    while len(received) < size:
        chunk = connection.recv(size - len(received))
        assert chunk, "unexpected socket EOF"
        received.extend(chunk)
    return bytes(received)


@pytest.mark.parametrize("credential", ["controller_pid", "controller_uid", "controller_gid"])
def test_wrong_peer_never_grants(session, credential):
    controller, connection = session.controller()
    wrong_identity = {"controller_pid": os.getpid(), "controller_uid": os.geteuid() + 1,
                      "controller_gid": os.getegid() + 1}
    connection.sendall(HELLO)
    result = session.start(controller, **{credential: wrong_identity[credential]})
    session.assert_receipt(result, "unauthorized_peer", granted=False)


def test_bad_hello_never_grants(session):
    controller, connection = session.controller()
    connection.sendall(b"MQPW2\n")
    result = session.start(controller)
    session.assert_receipt(result, "bad_hello", granted=False)


def test_fragmented_hello_preserves_native_frames_and_grants_once(session):
    controller, connection = session.controller()
    connection.sendall(HELLO[:2])
    result = session.start(controller)
    assert session.listener.received.wait(WAIT)
    assert session.drivers == [] and session.lease.mutations == []

    payload = bytes(range(256)) * 32 + HELLO
    frame = struct.pack("<I", len(payload)) + payload
    connection.sendall(HELLO[2:] + frame)
    assert receive(connection, len(ACK) + len(frame)) == ACK + frame
    connection.sendall(frame)
    assert receive(connection, len(frame)) == frame
    connection.close()
    session.assert_receipt(result, "ipc_eof", granted=True)


@pytest.mark.parametrize("borrow_pidfd", [False, True], ids=["owned", "borrowed"])
def test_controller_death_revokes_with_socket_still_open(session, borrow_pidfd):
    controller, connection = session.controller()
    connection.sendall(HELLO)
    identity = {"controller_pidfd": session.pidfd(controller.pid)} if borrow_pidfd else {}
    result = session.start(controller, **identity)
    assert receive(connection, len(ACK)) == ACK
    controller.kill()
    session.assert_receipt(result, "controller_exit", granted=True)
    assert controller.wait(timeout=WAIT) == -signal.SIGKILL
    assert connection.recv(1) == b""
    if borrow_pidfd:
        os.fstat(identity["controller_pidfd"])


@pytest.mark.parametrize("invalid", ["closed", "mismatched"])
def test_invalid_borrowed_pidfd_is_rejected_before_accept(session, invalid):
    controller, connection = session.controller()
    connection.sendall(HELLO)
    if invalid == "closed":
        descriptor = os.pidfd_open(controller.pid)
        os.close(descriptor)
        expected_error = OSError
    else:
        descriptor = session.pidfd(os.getpid())
        expected_error = ValueError

    result = session.start(controller, controller_pidfd=descriptor)
    with pytest.raises(expected_error) as failure:
        result.result(timeout=WAIT + 3)
    if invalid == "closed":
        assert failure.value.errno == errno.EBADF
    else:
        os.fstat(descriptor)
    assert session.listener.accepted is None
    assert session.drivers == [] and session.lease.mutations == []


def test_dead_borrowed_pidfd_is_rejected_before_accept(session):
    controller, connection = session.controller()
    connection.sendall(HELLO)
    descriptor = session.pidfd(controller.pid)
    controller.kill()
    assert controller.wait(timeout=WAIT) == -signal.SIGKILL

    result = session.start(controller, controller_pidfd=descriptor)
    session.assert_receipt(result, "controller_exit", granted=False)
    assert session.listener.accepted is None
    os.fstat(descriptor)


def test_controller_death_during_grant_revokes_without_ack(session):
    controller, connection = session.controller()
    connection.sendall(HELLO)

    def kill_controller():
        controller.kill()
        assert controller.wait(timeout=WAIT) == -signal.SIGKILL

    session.lease.after_add = kill_controller
    result = session.start(controller)
    session.assert_receipt(result, "controller_exit", granted=True)
    assert connection.recv(1) == b""


def test_ipc_eof_revokes_before_transport_close_or_driver_signal(session, monkeypatch):
    controller, connection = session.controller()
    connection.sendall(HELLO)
    result = session.start(controller, code=PAUSED_DRIVER)
    assert receive(connection, len(ACK)) == ACK
    driver, = session.drivers
    terminate = driver.terminate
    order = []

    def before_flush():
        assert session.listener.accepted.fileno() >= 0
        assert not driver.stdin.closed and not driver.stdout.closed
        assert driver.poll() is None
        order.append("flush")

    def terminate_after_flush():
        assert not session.lease.eligible()
        order.append("terminate")
        terminate()

    session.lease.before_flush = before_flush
    monkeypatch.setattr(driver, "terminate", terminate_after_flush)
    connection.shutdown(socket.SHUT_WR)
    session.assert_receipt(result, "ipc_eof", granted=True)
    assert order == ["flush", "terminate"]
    assert controller.poll() is None


def test_idle_session_times_out_and_revokes(session):
    controller, connection = session.controller()
    connection.sendall(HELLO)
    result = session.start(controller, timeout=1)
    assert receive(connection, len(ACK)) == ACK
    session.assert_receipt(result, "timeout", granted=True)
    assert controller.poll() is None


def test_driver_exit_revokes_with_full_stdin_pipe(session):
    controller, connection = session.controller()
    connection.sendall(HELLO)
    result = session.start(controller, code=PAUSED_DRIVER)
    assert receive(connection, len(ACK)) == ACK
    driver, = session.drivers
    capacity = fcntl.fcntl(driver.stdin, fcntl.F_SETPIPE_SZ, 4096)
    connection.sendall(b"x" * (2 * capacity))
    queued = array.array("i", [0])
    deadline = time.monotonic() + WAIT / 2
    while True:
        fcntl.ioctl(driver.stdin, termios.FIONREAD, queued)
        if queued[0] == capacity:
            break
        assert time.monotonic() < deadline, "driver stdin never became backpressured"
        time.sleep(0.01)

    driver.kill()
    session.assert_receipt(result, "driver_exit", granted=True)
    assert controller.poll() is None


def test_ipc_half_close_revokes_when_controller_hits_backpressure(session):
    controller, connection = session.controller()
    connection.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 8192)
    connection.sendall(HELLO)
    result = session.start(controller, code="import signal\nsignal.pause()")
    assert receive(connection, len(ACK)) == ACK
    driver, = session.drivers
    fcntl.fcntl(driver.stdin, fcntl.F_SETPIPE_SZ, 4096)

    connection.settimeout(0.1)
    chunk = b"x" * 4096
    with pytest.raises(TimeoutError):
        for _ in range(256):
            connection.sendall(chunk)
    connection.shutdown(socket.SHUT_WR)
    session.assert_receipt(result, "ipc_eof", granted=True)
    assert controller.poll() is None


def test_partial_grant_failure_flushes_and_cleans_up(session):
    controller, connection = session.controller()
    connection.sendall(HELLO)
    error = OSError("injected grant failure after add")
    session.lease.grant_error = error
    result = session.start(controller)
    with pytest.raises(OSError) as failure:
        result.result(timeout=WAIT + 3)
    assert failure.value is error
    assert session.lease.mutations == ["add", "flush"]
    assert not session.lease.eligible()
    assert connection.recv(1) == b""
    driver, = session.drivers
    assert driver.poll() is not None
    assert driver.stdin.closed and driver.stdout.closed


def test_unconfirmed_revoke_preserves_transport_for_recovery(session):
    controller, connection = session.controller()
    connection.sendall(HELLO)
    result = session.start(controller)
    assert receive(connection, len(ACK)) == ACK
    error = OSError("injected flush failure")
    session.lease.flush_error = error
    connection.shutdown(socket.SHUT_WR)
    with pytest.raises(RevocationError) as failure:
        result.result(timeout=WAIT + 3)
    driver, = session.drivers
    assert failure.value.__cause__ is error
    assert failure.value.driver is driver
    assert failure.value.connection is session.listener.accepted
    assert session.lease.mutations == ["add", "flush"] and session.lease.eligible()
    assert driver.poll() is None
    assert not driver.stdin.closed and not driver.stdout.closed
    assert session.listener.accepted.fileno() >= 0
