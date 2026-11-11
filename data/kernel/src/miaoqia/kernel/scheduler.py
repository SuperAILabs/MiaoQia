"""Periodic job runner for Python process roles; same semantics as the Go kernel/scheduler.

Subsystems declare jobs in runtime.Module.jobs; a role runs them when its roles.yaml entry sets ``jobs: true``.
Each job loops on its own thread: it runs once at start, then every ``every`` seconds; a round still running
when the next tick arrives makes that tick skip (no overlap). A round gets a deadline (``timeout``, default
``every``) that the job must honour cooperatively. With active/standby instances, a round first takes a
session-level advisory lock on the job's subsystem database and skips when another instance holds it. Jobs must
still be repeatable and safe to run concurrently; the lock only avoids duplicate work. A failed round is logged
and counted, never fatal.
"""
from __future__ import annotations

import logging
import re
import threading
import time
from dataclasses import dataclass
from typing import Callable

from sqlalchemy import text

from . import db

log = logging.getLogger("miaoqia.scheduler")
NAME = re.compile(r"^[a-z][a-z0-9-]*\.[a-z][a-z0-9-]*$")
RAN, SKIPPED, FAILED = "ran", "skipped", "failed"


@dataclass(frozen=True)
class Job:
    name: str                                # <subsystem>.<task>, unique in the process
    every: float                             # period in seconds, at least 1
    run: Callable[[float], int]              # receives a time.monotonic() deadline; returns items processed
    timeout: float = 0.0                     # round limit in seconds; 0 means every
    database: db.Database | None = None      # subsystem database for the instance lock; None means no lock


def validate(jobs: list[Job]) -> None:
    seen: set[str] = set()
    for j in jobs:
        if not NAME.fullmatch(j.name):
            raise ValueError(f"scheduler: invalid job name {j.name!r} (want <subsystem>.<task>)")
        if j.name in seen:
            raise ValueError(f"scheduler: duplicate job {j.name}")
        if j.every < 1:
            raise ValueError(f"scheduler: job {j.name} period must be at least 1 second")
        if j.timeout < 0 or j.timeout > j.every:
            raise ValueError(f"scheduler: job {j.name} timeout must be within its period")
        seen.add(j.name)


class Runner:
    def __init__(self, jobs: list[Job], *, on_round: Callable[[str, str, int, Exception | None], None] | None = None):
        validate(jobs)
        self.jobs = jobs
        self.on_round = on_round

    def run(self, stop: threading.Event) -> None:
        threads = [threading.Thread(target=self._loop, args=(j, stop), name=j.name, daemon=True) for j in self.jobs]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

    def _loop(self, job: Job, stop: threading.Event) -> None:
        next_at = time.monotonic()
        while not stop.is_set():
            self.once(job)
            next_at += job.every
            now = time.monotonic()
            if next_at < now:                  # rounds longer than the period drop the missed ticks
                next_at = now + job.every - ((now - next_at) % job.every)
            stop.wait(next_at - now)

    def once(self, job: Job) -> str:
        """Run one round (lock → run → unlock). Exposed for tests and manual triggering."""
        deadline = time.monotonic() + (job.timeout or job.every)
        n, err = 0, None
        try:
            outcome, n = self._locked(job, deadline)
        except Exception as e:  # noqa: BLE001 — a failed round must not stop the runner
            outcome, err = FAILED, e
            log.warning("scheduled job failed job=%s error=%s", job.name, e)
        if outcome == RAN and n:
            log.info("scheduled job job=%s processed=%d", job.name, n)
        if self.on_round:
            self.on_round(job.name, outcome, n, err)
        return outcome

    @staticmethod
    def _locked(job: Job, deadline: float) -> tuple[str, int]:
        if job.database is None:
            return RAN, job.run(deadline)
        key = f"scheduler/{job.name}"
        with job.database.engine.connect().execution_options(isolation_level="AUTOCOMMIT") as conn:
            if not conn.execute(text("SELECT pg_try_advisory_lock(hashtextextended(:k, 0))"), {"k": key}).scalar_one():
                return SKIPPED, 0
            try:
                return RAN, job.run(deadline)
            finally:
                try:
                    conn.execute(text("SELECT pg_advisory_unlock(hashtextextended(:k, 0))"), {"k": key})
                except Exception:  # noqa: BLE001 — closing the session releases the lock
                    conn.invalidate()
