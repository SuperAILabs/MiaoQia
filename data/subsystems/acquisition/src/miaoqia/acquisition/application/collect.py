"""Scheduled incremental collection: one periodic job per declared source with a schedule.

Each job runs Fetcher.run over the source's lookback window ending today (China date) with the round deadline
from the scheduler. Archived details are skipped, so a round only fetches new announcements. A blocked source
stays stopped (the round logs and does nothing) until an operator reviews it and unblocks it with
mq-acquisition-fetch --unblock. Collection runs as kernel periodic jobs in the acq-scheduler role; backfill
workflows are not part of this.
"""
from __future__ import annotations

import logging
from datetime import timedelta

from miaoqia.kernel import db, scheduler

from ..api import NAME
from ..api.handlers import Window
from .fetch import Fetcher, SourceStopped, china_today
from .sources import DeclaredSource

log = logging.getLogger("miaoqia.acquisition.collect")


def jobs(fetcher: Fetcher, database: db.Database, declared: dict[str, DeclaredSource]) -> list[scheduler.Job]:
    return [_job(fetcher, database, src) for src in declared.values() if src.schedule is not None]


def _job(fetcher: Fetcher, database: db.Database, src: DeclaredSource) -> scheduler.Job:
    schedule = src.schedule

    def run(deadline: float) -> int:
        today = china_today()
        window = Window(today - timedelta(days=schedule.lookback_days), today)
        try:
            report = fetcher.run(src, window, max_pages=schedule.max_pages, deadline=deadline)
        except SourceStopped:
            log.warning("source blocked, skipping scheduled collection source=%s", src.source_id)
            return 0
        if report.blocked or report.failed:
            log.warning("collection source=%s health=%s failed=%d blocked=%s", src.source_id, report.health,
                        report.failed, report.blocked)
        return report.archived

    return scheduler.Job(name=f"{NAME}.collect-{src.source_id}", every=schedule.every_minutes * 60, run=run,
                         database=database)
