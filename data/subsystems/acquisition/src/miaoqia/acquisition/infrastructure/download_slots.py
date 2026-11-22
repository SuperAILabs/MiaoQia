"""PostgreSQL admission for HTTP downloads shared by scheduler and manual collectors."""
from __future__ import annotations

from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime, timedelta

from sqlalchemy import Connection, text

from ..api.handlers import FetchRequest

_WAIT_FOR_SLOT = 0.05
_BACKFILL_SLOT = 3
_LOCK = text("SELECT pg_try_advisory_lock(hashtextextended(:key, 0))")
_UNLOCK = text("SELECT pg_advisory_unlock(hashtextextended(:key, 0))")


@dataclass
class DownloadSlot:
    connection: Connection
    scopes: tuple[str, str]
    site: str
    now: Callable[[], datetime | None]

    def availability(self, interval: float) -> tuple[datetime, float]:
        now = self.connection.execute(
            text("SELECT COALESCE(CAST(:now AS timestamptz), clock_timestamp())"),
            {"now": self.now()},
        ).scalar_one()
        delay = self.connection.execute(text("""
            SELECT max(EXTRACT(epoch FROM (greatest(ready_at, sent_at + :spacing * interval '1 second') - :now)))
            FROM acq_download_pacing WHERE scope IN (:source, :site)
        """), {"source": self.scopes[0], "site": self.scopes[1], "spacing": interval, "now": now}).scalar_one()
        return now, max(0.0, float(delay or 0))

    def dispatch(self, request: FetchRequest, *, run_id: int, generation: int,
                 at: datetime, interval: float, task_key: str | None = None) -> int:
        self.connection.execute(text("""
            INSERT INTO acq_download_pacing (scope, sent_at, ready_at) VALUES (:scope, :now, :ready)
            ON CONFLICT (scope) DO UPDATE SET sent_at = EXCLUDED.sent_at, ready_at = EXCLUDED.ready_at
        """), [{"scope": scope, "now": at, "ready": at + timedelta(seconds=interval)} for scope in self.scopes])
        return self.connection.execute(text("""
            INSERT INTO acq_download_starts (run_id, generation, fingerprint, site, task_key)
            VALUES (:run, :generation, :fingerprint, :site, :task) RETURNING id
        """), {"run": run_id, "generation": generation, "fingerprint": request.fingerprint(),
               "site": self.site, "task": task_key}).scalar_one()

    def cooldown(self, delay: float, *, shared_site: bool = False) -> None:
        with self.connection.begin():
            self.connection.execute(text("""
                UPDATE acq_download_pacing SET ready_at = greatest(
                    ready_at, COALESCE(CAST(:now AS timestamptz), clock_timestamp()) + :delay * interval '1 second')
                WHERE scope IN (:source, :site)
            """), {"source": self.scopes[0], "site": self.scopes[1] if shared_site else None,
                   "now": self.now(), "delay": delay})


class DownloadSlots:
    """One transfer per source and site, three online slots and one reserved for backfill.

    Lock ownership lasts through transport, including response reads. Process exit frees the locks.
    Ready times persist across process exits and use the database clock in production.
    """

    def __init__(self, database, *, now: Callable[[], datetime | None] = lambda: None):
        self.database = database
        self.now = now

    @contextmanager
    def try_acquire(self, *, source: str, site: str, mode: str) -> Iterator[DownloadSlot | float]:
        scopes = (f"source/{source}", f"site/{site}")
        with self.database.engine.connect() as conn:
            acquired = []

            def take(key: str) -> bool:
                if conn.execute(_LOCK, {"key": key}).scalar_one():
                    acquired.append(key)
                    return True
                return False

            try:
                with conn.begin():
                    scoped = all(take(f"acq/download/{scope}") for scope in scopes)
                    indices = (_BACKFILL_SLOT,) if mode == "backfill" else range(_BACKFILL_SLOT)
                    admitted = scoped and any(take(f"acq/download/slot/{index}") for index in indices)
                if admitted:
                    yield DownloadSlot(conn, scopes, site, self.now)
            finally:
                try:
                    if acquired and not conn.invalidated:
                        with conn.begin():
                            for key in reversed(acquired):
                                conn.execute(_UNLOCK, {"key": key})
                except BaseException:
                    # Pool reuse must never retain session locks after cleanup fails.
                    conn.invalidate()
                    raise
        if not admitted:
            yield _WAIT_FOR_SLOT
