"""Keep the initial date scan and its retry ledger tied to committed collection outcomes."""
from __future__ import annotations

import logging
from datetime import date, timedelta

from sqlalchemy import Connection, text

from .coverage import unverified_channels

log = logging.getLogger(__name__)
ARCHIVE_BOUNDARIES = frozenset({"rolling_archive_boundary", "pagination_requires_captcha"})
GROUP_WINDOW_UNVERIFIED = "section_notice_coverage_unverified"
LIST_DATE_UNAVAILABLE = "list_publication_date_unavailable"
SOURCE_BOUNDARIES = ARCHIVE_BOUNDARIES | {"homepage_current_window", GROUP_WINDOW_UNVERIFIED, LIST_DATE_UNAVAILABLE}


def waits_for_source_change(reasons: set[str]) -> bool:
    if GROUP_WINDOW_UNVERIFIED in reasons:
        reasons = reasons - {"page_limit"}
    if LIST_DATE_UNAVAILABLE in reasons:
        reasons = reasons - {"pagination_unverified"}
    return bool(reasons) and reasons <= SOURCE_BOUNDARIES


def next_day(conn: Connection, source: str, today: date, days: int, fingerprint: str) -> date | None:
    conn.execute(text("""
        INSERT INTO acq_initial_backfill (source_id, oldest_day, next_day) VALUES (:s, :oldest, :previous)
        ON CONFLICT (source_id) DO NOTHING
    """), {"s": source, "oldest": today - timedelta(days=days - 1), "previous": today - timedelta(days=1)})
    return conn.execute(text("""
        SELECT COALESCE(next_day, (
            SELECT g.day FROM acq_initial_backfill_gaps g
            JOIN acq_collection_runs r ON r.id = g.last_run_id
            LEFT JOIN acq_collection_plans p ON p.run_id = r.id
            WHERE g.source_id = :s AND NOT COALESCE(
                r.report->>'wait_for_source_change' = 'true' AND p.config_fingerprint = :fingerprint,
                false
            )
            ORDER BY g.attempted_at, g.day DESC LIMIT 1
        )) FROM acq_initial_backfill WHERE source_id = :s
    """), {"s": source, "fingerprint": fingerprint}).scalar_one()


def settle(conn: Connection, source: str, day: date, run_id: int, channels: tuple[str, ...], *,
           incomplete: bool) -> list[str]:
    """Called with the source lease fenced, in the same transaction as its run report."""
    params = {"s": source, "day": day, "run": run_id}
    expected = conn.execute(text("""
        SELECT 1 FROM acq_initial_backfill
        WHERE source_id = :s AND (next_day = :day OR EXISTS (
            SELECT 1 FROM acq_initial_backfill_gaps WHERE source_id = :s AND day = :day
        )) FOR UPDATE
    """), params).first()
    if expected is None:
        return []
    unverified = [] if incomplete else unverified_channels(conn, source, channels, run_id)
    if incomplete or unverified:
        log.warning("initial backfill pending source=%s date=%s run_id=%s", source, day, run_id)
        conn.execute(text("""
            INSERT INTO acq_initial_backfill_gaps (source_id, day, last_run_id)
            VALUES (:s, :day, :run)
            ON CONFLICT (source_id, day) DO UPDATE
            SET last_run_id = EXCLUDED.last_run_id, attempted_at = clock_timestamp()
            WHERE acq_initial_backfill_gaps.last_run_id <= EXCLUDED.last_run_id
        """), params)
    else:
        conn.execute(text("""
            DELETE FROM acq_initial_backfill_gaps
            WHERE source_id = :s AND day = :day AND last_run_id <= :run
        """), params)
    conn.execute(text("""
        UPDATE acq_initial_backfill SET next_day = CASE WHEN next_day > oldest_day THEN next_day - 1 END
        WHERE source_id = :s AND next_day = :day
    """), params)
    return unverified
