from __future__ import annotations

import signal
import threading
from unittest.mock import Mock

import pytest

from miaoqia.kernel import runtime


@pytest.fixture
def runtime_env(monkeypatch, tmp_path):
    unit = tmp_path / "test"
    unit.mkdir()
    (unit / "roles.yaml").write_text("roles:\n  consumer:\n    subsystems: [test]\n", encoding="utf-8")
    monkeypatch.setenv("MIAOQIA_DEPLOY_DIR", str(tmp_path))
    monkeypatch.setattr(runtime.tracing, "setup", Mock())
    shutdown = Mock()
    monkeypatch.setattr(runtime.tracing, "shutdown", shutdown)
    handlers = {}
    monkeypatch.setattr(runtime.signal, "signal", lambda sig, handler: handlers.update({sig: handler}))
    return shutdown, handlers


@pytest.mark.parametrize("raises", [True, False])
def test_consumer_exit_stops_siblings_and_fails_runtime(runtime_env, raises):
    shutdown, handlers = runtime_env
    started, stopped, finished = threading.Event(), threading.Event(), threading.Event()
    error = RuntimeError("commit failed")

    def sibling_run():
        started.set()
        assert stopped.wait(10)
        finished.set()

    def failed_run():
        assert started.wait(5)
        if raises:
            raise error

    sibling = Mock(name="sibling")
    sibling.name = "sibling"
    sibling.run.side_effect = sibling_run
    sibling.stop.side_effect = stopped.set
    failed = Mock(name="failed")
    failed.name = "failed"
    failed.run.side_effect = failed_run
    module = runtime.Module(name="test", consumers=lambda _: [sibling, failed])
    # A broken supervisor must fail the test instead of waiting forever for a signal.
    watchdog = threading.Timer(5, lambda: handlers[signal.SIGTERM](signal.SIGTERM, None))
    watchdog.start()
    try:
        with pytest.raises(RuntimeError, match="failed") as exc:
            runtime.run("test", "consumer", [module])
    finally:
        watchdog.cancel()
    if raises:
        assert exc.value.__cause__ is error
    assert finished.is_set()
    sibling.stop.assert_called_once()
    failed.stop.assert_called_once()
    shutdown.assert_called_once()


def test_signal_shutdown_is_successful(runtime_env):
    shutdown, handlers = runtime_env
    c = Mock()
    c.name = "test"
    c.run.side_effect = lambda: handlers[signal.SIGTERM](signal.SIGTERM, None)
    runtime.run("test", "consumer", [runtime.Module(name="test", consumers=lambda _: [c])])
    c.stop.assert_called_once()
    shutdown.assert_called_once()
