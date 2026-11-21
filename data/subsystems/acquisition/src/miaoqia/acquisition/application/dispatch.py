"""Connect the durable plan ledger to the scheduler and fixed fetcher workers."""
from __future__ import annotations

import logging
import time
from datetime import datetime

from sqlalchemy import text

from miaoqia.kernel import db, reqctx, scheduler

from ..api import NAME
from . import plans, sources
from .fetch import Fetcher, RunReport

log = logging.getLogger(__name__)


def planner_job(database: db.Database, declared: dict[str, sources.DeclaredSource]) -> scheduler.Job:
    def run(deadline: float) -> int:
        created = 0
        with reqctx.use(reqctx.service(NAME)):
            for source in declared.values():
                if time.monotonic() >= deadline:
                    break
                with database.tx() as conn:
                    now = conn.execute(text("SELECT clock_timestamp()")).scalar_one()
                    created += plans.materialize(conn, source, now)
        return created

    return scheduler.Job(f"{NAME}.plan-windows", every=15, run=run)


def dispatch_once(fetcher: Fetcher, declared: dict[str, sources.DeclaredSource], fingerprints: str,
                  *, mode: str, prefer_recovery: bool = False, now: datetime | None = None) -> RunReport | None:
    with reqctx.use(reqctx.service(NAME)), fetcher.db.tx() as conn:
        now = now if now is not None else conn.execute(text("SELECT clock_timestamp()")).scalar_one()
        claimed = plans.claim(conn, fingerprints, now, mode=mode, prefer_recovery=prefer_recovery)
    if claimed is None:
        return None
    lease, budget = claimed
    return fetcher.execute_claimed(declared[lease.source_id], lease,
                                   deadline=fetcher.clock.monotonic() + budget)


def workers(fetcher: Fetcher, declared: dict[str, sources.DeclaredSource], *, browser_only: bool = False):
    from miaoqia.kernel.runtime import Worker

    fingerprints = plans.fingerprints_json(declared)

    def worker(name: str, mode: str, *, prefer_recovery: bool = False, allow_backfill: bool = False) -> Worker:
        def run(stop) -> None:
            while not stop.is_set():
                result = dispatch_once(fetcher, declared, fingerprints, mode=mode, prefer_recovery=prefer_recovery)
                if result is None and allow_backfill:
                    result = dispatch_once(fetcher, declared, fingerprints, mode="backfill", prefer_recovery=True)
                if result is None:
                    stop.wait(2)
                elif result.blocked or result.failed or result.gaps:
                    log.warning("collection source=%s run=%s failed=%d blocked=%s gaps=%d",
                                result.source_id, result.run_id, result.failed, result.blocked, len(result.gaps))

        return Worker(name=f"{NAME}.{name}", run=run)

    if browser_only:
        return [worker("browser-collect", "incremental", prefer_recovery=True, allow_backfill=True)]
    return [worker("fetch-online-1", "incremental"), worker("fetch-online-2", "incremental"),
            worker("fetch-recovery", "incremental", prefer_recovery=True), worker("fetch-history", "backfill")]
