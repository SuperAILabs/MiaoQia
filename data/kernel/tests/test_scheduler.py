from __future__ import annotations

import signal
import threading
import time

import pytest
from sqlalchemy import text

from miaoqia.kernel import db, runtime, scheduler


def job(name="test.job", every=60.0, run=lambda deadline: 1, **kw):
    return scheduler.Job(name=name, every=every, run=run, **kw)


@pytest.mark.parametrize("bad", [
    [job(name="nodot")], [job(), job()], [job(every=0.5)], [job(timeout=120)], [job(timeout=-1)],
])
def test_validate_rejects_bad_declarations(bad):
    with pytest.raises(ValueError):
        scheduler.validate(bad)


def test_once_reports_ran_and_failed_with_deadline():
    seen = []

    def run(deadline):
        seen.append(deadline - time.monotonic())
        return 3

    rounds = []
    r = scheduler.Runner([job(run=run, timeout=5), job(name="test.broken", run=lambda d: 1 / 0)],
                         on_round=lambda *a: rounds.append(a))
    assert r.once(r.jobs[0]) == scheduler.RAN and 4 < seen[0] <= 5
    assert r.once(r.jobs[1]) == scheduler.FAILED
    assert rounds[0][:3] == ("test.job", "ran", 3) and isinstance(rounds[1][3], ZeroDivisionError)


def test_lock_held_elsewhere_skips_round(database):
    d = db.Database(database["app"])
    calls = []
    j = job(run=lambda deadline: calls.append(1) or 1, database=d)
    r = scheduler.Runner([j])
    with d.engine.connect().execution_options(isolation_level="AUTOCOMMIT") as other:
        other.execute(text("SELECT pg_advisory_lock(hashtextextended('scheduler/test.job', 0))"))
        assert r.once(j) == scheduler.SKIPPED and calls == []
        other.execute(text("SELECT pg_advisory_unlock(hashtextextended('scheduler/test.job', 0))"))
    assert r.once(j) == scheduler.RAN and calls == [1]
    # The lock is released after the round, so another session can take it.
    with d.engine.connect() as c:
        assert c.execute(text("SELECT pg_try_advisory_lock(hashtextextended('scheduler/test.job', 0))")).scalar_one()
    d.close()


def test_runner_runs_immediately_and_stops():
    ran = threading.Event()
    stop = threading.Event()
    r = scheduler.Runner([job(run=lambda d: ran.set() or 0)])
    t = threading.Thread(target=r.run, args=(stop,))
    t.start()
    assert ran.wait(5)
    stop.set()
    t.join(5)
    assert not t.is_alive()


def test_runtime_runs_jobs_only_in_job_roles(monkeypatch, tmp_path):
    unit = tmp_path / "test"
    unit.mkdir()
    (unit / "roles.yaml").write_text("roles:\n  sched:\n    subsystems: [test]\n    jobs: true\n  plain:\n    subsystems: [test]\n")
    (tmp_path / "subsystems.yaml").write_text("subsystems:\n  test: {unit: test, data_scopes: [public]}\n")
    monkeypatch.setenv("MIAOQIA_DEPLOY_DIR", str(tmp_path))
    monkeypatch.setattr(runtime.tracing, "setup", lambda *a: None)
    monkeypatch.setattr(runtime.tracing, "shutdown", lambda: None)
    handlers = {}
    monkeypatch.setattr(runtime.signal, "signal", lambda sig, handler: handlers.update({sig: handler}))
    ran = []

    def run(deadline):
        ran.append(1)
        handlers[signal.SIGTERM](signal.SIGTERM, None)
        return 0

    module = runtime.Module(name="test", jobs=lambda env: [job(run=run)])
    runtime.run("test", "sched", [module])
    assert ran == [1]
    watchdog = threading.Timer(0.5, lambda: handlers[signal.SIGTERM](signal.SIGTERM, None))
    watchdog.start()
    runtime.run("test", "plain", [module])
    watchdog.cancel()
    assert ran == [1]
