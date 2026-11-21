"""Own one durable collection window per source at a time."""
from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime

from sqlalchemy import Connection, text

from ..api.handlers import Window

_LEASE_SECONDS = 150
CONTRACT_CHANGED = "source_contract_changed"


class LeaseLost(RuntimeError):
    pass


@dataclass(frozen=True)
class CollectionLease:
    source_id: str
    run_id: int
    generation: int
    mode: str
    window: Window
    max_pages: int
    refetch: bool

    def check(self, conn: Connection) -> None:
        """Lock the source in the same transaction as any authoritative write."""
        held = conn.execute(text("""
            UPDATE acq_sources SET lease_until = clock_timestamp() + :seconds * interval '1 second'
            WHERE source_id = :source AND active_run_id = :run AND lease_generation = :generation
              AND lease_until > clock_timestamp()
            RETURNING 1
        """), {"source": self.source_id, "run": self.run_id, "generation": self.generation,
               "seconds": _LEASE_SECONDS}).first()
        if held is None:
            raise LeaseLost(self.source_id)

    @contextmanager
    def writable(self, database) -> Iterator[Connection]:
        with database.tx() as conn:
            self.check(conn)
            yield conn

    def release(self, conn: Connection) -> None:
        released = conn.execute(text("""
            UPDATE acq_sources SET active_run_id = NULL, lease_generation = NULL, lease_until = NULL
            WHERE source_id = :source AND active_run_id = :run AND lease_generation = :generation
            RETURNING 1
        """), {"source": self.source_id, "run": self.run_id, "generation": self.generation}).first()
        if released is None:
            raise LeaseLost(self.source_id)


def retire(conn: Connection, source_id: str, run_id: int) -> None:
    """Close an interrupted run after the caller has locked its source and checked lease expiry."""
    params = {"source": source_id, "run": run_id, "reason": CONTRACT_CHANGED}
    conn.execute(text("""
        UPDATE acq_collection_runs
        SET finished_at = clock_timestamp(), pending_report = NULL,
            report = COALESCE(pending_report, '{}'::jsonb) || jsonb_build_object('termination_reason', CAST(:reason AS text))
        WHERE source_id = :source AND id = :run AND finished_at IS NULL
    """), params)
    conn.execute(text("""
        UPDATE acq_sources SET active_run_id = NULL, lease_generation = NULL, lease_until = NULL
        WHERE source_id = :source AND active_run_id = :run AND lease_until <= clock_timestamp()
    """), params)


def claim(conn: Connection, source: str, window: Window, *, max_pages: int, refetch: bool,
          mode: str, started_at: datetime, recover: bool, resume_id: int | None = None,
          fresh: bool = False) -> CollectionLease | None:
    """The caller holds the source row lock."""
    busy = conn.execute(text("""
        SELECT lease_until > clock_timestamp() FROM acq_sources WHERE source_id = :source
    """), {"source": source}).scalar_one()
    if busy:
        return None
    plan = {"source": source, "mode": mode, "start": window.since, "end": window.until,
            "pages": max_pages, "refetch": refetch, "now": started_at, "recover": recover, "resume_id": resume_id}
    old = None if fresh else conn.execute(text("""
        SELECT id, window_start, window_end, max_pages, refetch FROM acq_collection_runs
        WHERE source_id = :source AND mode = :mode AND finished_at IS NULL AND generation > 0
          AND (
              id = CAST(:resume_id AS bigint)
              OR (CAST(:resume_id AS bigint) IS NULL AND (:recover OR (
                  window_start = :start AND window_end = :end AND max_pages = :pages AND refetch = :refetch
              )))
          )
        ORDER BY id LIMIT 1
    """), plan).first()
    if old is None:
        if resume_id is not None:
            return None
        run_id = conn.execute(text("""
            INSERT INTO acq_collection_runs
                (source_id, window_start, window_end, max_pages, refetch, started_at, mode, generation)
            VALUES (:source, :start, :end, :pages, :refetch, :now, :mode, 1) RETURNING id
        """), plan).scalar_one()
        generation = 1
    else:
        run_id = old.id
        window = Window(old.window_start, old.window_end)
        max_pages, refetch = old.max_pages, old.refetch
        generation = conn.execute(text("""
            UPDATE acq_collection_runs SET generation = generation + 1 WHERE id = :run RETURNING generation
        """), {"run": run_id}).scalar_one()
    conn.execute(text("""
        UPDATE acq_sources SET active_run_id = :run, lease_generation = :generation,
            lease_until = clock_timestamp() + :seconds * interval '1 second'
        WHERE source_id = :source
    """), {"source": source, "run": run_id, "generation": generation, "seconds": _LEASE_SECONDS})
    return CollectionLease(source, run_id, generation, mode, window, max_pages, refetch)
