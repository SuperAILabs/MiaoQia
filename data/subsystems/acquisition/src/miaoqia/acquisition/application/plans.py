"""Durable scheduling and claims. Source leases remain the execution fence."""
from __future__ import annotations

import json
from datetime import date, datetime, time, timedelta, timezone

from sqlalchemy import Connection, text

from ..api.handlers import Window
from . import collection_leases, initial_backfill, sources

CHINA = timezone(timedelta(hours=8))
RETRY_DELAY_SECONDS = 60


def materialize(conn: Connection, source: sources.DeclaredSource, now: datetime) -> int:
    schedule = source.schedule
    if schedule is None:
        return 0
    sid = source.source_id
    fingerprint = sources.config_fingerprint(source)
    with conn.begin_nested() as registration:
        state = sources.register(conn, source)
        current, busy = conn.execute(text("""
            SELECT collection_fingerprint, lease_until > clock_timestamp() FROM acq_sources WHERE source_id = :s
        """), {"s": sid}).one()
        if (busy and current != fingerprint) or state == "blocked" or not conn.execute(
            text("SELECT public.acq_collection_allowed(:s)"), {"s": sid},
        ).scalar_one():
            registration.rollback()
            return 0

    timing = {"s": sid, "now": now}
    conn.execute(text("""
        INSERT INTO acq_collection_schedule (source_id, next_due_at) VALUES (:s, :now)
        ON CONFLICT (source_id) DO NOTHING
    """), timing)
    due = conn.execute(text("""
        SELECT next_due_at FROM acq_collection_schedule WHERE source_id = :s FOR UPDATE
    """), timing).scalar_one()

    online = schedule.every_minutes * 60
    historical = online // 5 if schedule.initial_backfill_days > 1 else 0
    today = now.astimezone(CHINA).date()
    created = 0
    switching = current != fingerprint
    if switching:
        created += _replace_stale(conn, source, fingerprint, online - historical, historical)
        conn.execute(text("UPDATE acq_sources SET collection_fingerprint = :f WHERE source_id = :s"),
                     {"s": sid, "f": fingerprint})
    created += _adopt_unfinished(conn, source, fingerprint, online - historical, historical,
                                 replace=switching and current is not None)
    if due <= now:
        latest = due + ((now - due) // timedelta(seconds=online)) * timedelta(seconds=online)
        if latest.astimezone(CHINA).date() != due.astimezone(CHINA).date():
            created += _plan_incremental(conn, source, fingerprint, latest, today, online - historical)
        created += _plan_incremental(conn, source, fingerprint, due, today, online - historical)
        following = _next_due(due, now, online)
        conn.execute(text("UPDATE acq_collection_schedule SET next_due_at = :following WHERE source_id = :s"),
                     {"s": sid, "following": following})

    if historical:
        day = initial_backfill.next_day(conn, sid, today, schedule.initial_backfill_days, fingerprint)
        if day is not None:
            created += _plan_backfill(conn, source, fingerprint, day, now, historical)
    return created


def _next_due(due: datetime, now: datetime, period: int) -> datetime:
    # Collapse missed repetitions within a civil day, while retaining each missed publication window.
    midnight = datetime.combine(due.astimezone(CHINA).date() + timedelta(days=1), time.min, CHINA)
    elapsed = min(now, midnight - timedelta(microseconds=1)) - due
    ticks = elapsed // timedelta(seconds=period) + 1
    return due + timedelta(seconds=ticks * period)


def _plan_incremental(conn: Connection, source: sources.DeclaredSource, fingerprint: str,
                      due: datetime, today: date, budget: int) -> int:
    day = due.astimezone(CHINA).date()
    window = Window(day - timedelta(days=source.schedule.lookback_days), day)
    represented = conn.execute(text("""
        SELECT 1 FROM acq_collection_plans
        WHERE source_id = :s AND mode = 'incremental' AND window_start = :start AND window_end = :end
          AND settlement_reason IS NULL
          AND config_fingerprint = :f AND (due_at >= :due OR (settled_at IS NULL AND (run_id IS NULL OR :old)))
        LIMIT 1
    """), {"s": source.source_id, "start": window.since, "end": window.until, "f": fingerprint,
           "due": due, "old": day < today}).first()
    if represented is not None:
        return 0
    return _insert(conn, source, fingerprint, "incremental", due.astimezone(timezone.utc).isoformat(), due,
                   window, source.schedule.max_pages, True, budget)


def _replace_stale(conn: Connection, source: sources.DeclaredSource, fingerprint: str,
                   online: int, historical: int) -> int:
    stale = conn.execute(text("""
        SELECT p.id, p.mode, p.occurrence_key, p.due_at, p.not_before, p.window_start, p.window_end,
               p.refetch, p.run_id, r.finished_at
        FROM acq_collection_plans p LEFT JOIN acq_collection_runs r ON r.id = p.run_id
        WHERE p.source_id = :s AND p.settled_at IS NULL AND p.config_fingerprint <> :f
        ORDER BY p.id
    """), {"s": source.source_id, "f": fingerprint}).all()
    created = 0
    for plan in stale:
        if plan.finished_at is not None:
            close_finished(conn, plan.run_id)
            continue
        if plan.run_id is not None:
            collection_leases.retire(conn, source.source_id, plan.run_id)
        conn.execute(text("""
            UPDATE acq_collection_plans SET settled_at = clock_timestamp(), settlement_reason = :reason WHERE id = :id
        """), {"id": plan.id, "reason": collection_leases.CONTRACT_CHANGED})
        pages, budget = _limits(source, plan.mode, online, historical)
        created += _insert(conn, source, fingerprint, plan.mode, plan.occurrence_key, plan.due_at,
                           Window(plan.window_start, plan.window_end), pages, plan.refetch, budget,
                           not_before=plan.not_before)
    return created


def _limits(source: sources.DeclaredSource, mode: str, online: int, historical: int) -> tuple[int, int]:
    if mode == "backfill":
        return source.schedule.backfill_max_pages, historical or online
    return source.schedule.max_pages, online


def _adopt_unfinished(conn: Connection, source: sources.DeclaredSource, fingerprint: str,
                      online: int, historical: int, *, replace: bool) -> int:
    pending = conn.execute(text("""
        SELECT r.id, r.mode, r.window_start, r.window_end, r.started_at, r.max_pages, r.refetch
        FROM acq_collection_runs r
        WHERE r.source_id = :s AND r.finished_at IS NULL AND r.generation > 0
          AND r.mode IN ('incremental', 'backfill')
          AND NOT EXISTS (SELECT 1 FROM acq_collection_plans p WHERE p.run_id = r.id)
        ORDER BY r.id LIMIT :limit
    """), {"s": source.source_id, "limit": None if replace else 16}).all()
    created = 0
    for run in pending:
        max_pages, budget = _limits(source, run.mode, online, historical)
        if replace:
            collection_leases.retire(conn, source.source_id, run.id)
        created += _insert(conn, source, fingerprint, run.mode, f"resume:{run.id}", run.started_at,
                           Window(run.window_start, run.window_end), max_pages if replace else run.max_pages,
                           run.refetch, budget, run_id=None if replace else run.id)
    return created


def _plan_backfill(conn: Connection, source: sources.DeclaredSource, fingerprint: str, day: date,
                   now: datetime, budget: int) -> int:
    open_plan = conn.execute(text("""
        SELECT 1 FROM acq_collection_plans
        WHERE source_id = :s AND mode = 'backfill' AND window_start = :day
          AND config_fingerprint = :f AND settled_at IS NULL LIMIT 1
    """), {"s": source.source_id, "day": day, "f": fingerprint}).first()
    if open_plan is not None:
        return 0
    state = conn.execute(text("""
        SELECT last_run_id FROM acq_initial_backfill_gaps WHERE source_id = :s AND day = :day
    """), {"s": source.source_id, "day": day}).scalar_one_or_none()
    return _insert(conn, source, fingerprint, "backfill", f"{day}:{state or 0}", now,
                   Window(day, day), source.schedule.backfill_max_pages, False, budget)


def _insert(conn: Connection, source: sources.DeclaredSource, fingerprint: str, mode: str, key: str,
            due: datetime, window: Window, max_pages: int, refetch: bool, budget: int,
            *, run_id: int | None = None, not_before: datetime | None = None) -> int:
    inserted = conn.execute(text("""
        INSERT INTO acq_collection_plans (source_id, mode, occurrence_key, due_at, not_before,
            window_start, window_end, max_pages, refetch, handler, config_fingerprint, budget_seconds, run_id)
        VALUES (:s, :mode, :key, :due, :not_before, :start, :end, :pages, :refetch, :handler, :f, :budget, :run)
        ON CONFLICT DO NOTHING RETURNING id
    """), {"s": source.source_id, "mode": mode, "key": key, "due": due, "not_before": not_before or due,
           "start": window.since, "end": window.until, "pages": max_pages, "refetch": refetch,
           "handler": source.handler_ref, "f": fingerprint, "budget": budget,
           "run": run_id}).scalar_one_or_none()
    return int(inserted is not None)


def claim(conn: Connection, fingerprints: str, now: datetime, *, mode: str,
          prefer_recovery: bool = False) -> tuple[collection_leases.CollectionLease, int] | None:
    today = now.astimezone(CHINA).date()
    plan_id = conn.execute(text("""
        SELECT p.id
        FROM acq_collection_plans p JOIN acq_sources s USING (source_id)
        WHERE p.mode = :mode AND p.settled_at IS NULL AND p.not_before <= :now
          AND s.state = 'active' AND (s.lease_until IS NULL OR s.lease_until <= clock_timestamp())
          AND p.config_fingerprint = s.collection_fingerprint
          AND p.config_fingerprint = (CAST(:fingerprints AS jsonb) ->> p.source_id)
          AND public.acq_collection_allowed(s.source_id)
          AND ((p.mode = 'incremental' AND p.run_id IS NULL AND p.window_end = :today) OR NOT EXISTS (
              SELECT 1 FROM acq_collection_plans current_window
              WHERE current_window.source_id = p.source_id AND current_window.mode = 'incremental'
                AND current_window.settled_at IS NULL AND current_window.not_before <= :now
                AND current_window.run_id IS NULL AND current_window.window_end = :today
                AND current_window.config_fingerprint = p.config_fingerprint
          ))
        ORDER BY CASE
            WHEN p.mode = 'backfill' THEN CASE WHEN p.run_id IS NULL THEN 0 ELSE 1 END
            WHEN (p.run_id IS NULL AND p.window_end = :today) = :prefer_recovery THEN 1
            ELSE 0 END,
            p.due_at, p.id
        LIMIT 1 FOR NO KEY UPDATE OF s SKIP LOCKED
    """), {"mode": mode, "now": now, "today": today, "fingerprints": fingerprints,
           "prefer_recovery": prefer_recovery}).scalar_one_or_none()
    if plan_id is None:
        return None
    row = conn.execute(text("""
        SELECT p.id, p.source_id, p.window_start, p.window_end, p.max_pages, p.refetch, p.budget_seconds, p.run_id
        FROM acq_collection_plans p JOIN acq_sources s USING (source_id)
        WHERE p.id = :id AND p.settled_at IS NULL AND p.not_before <= :now
          AND p.config_fingerprint = s.collection_fingerprint
    """), {"id": plan_id, "now": now}).first()
    if row is None:
        return None
    lease = collection_leases.claim(conn, row.source_id, Window(row.window_start, row.window_end),
                                     max_pages=row.max_pages, refetch=row.refetch, mode=mode, started_at=now,
                                     recover=False, resume_id=row.run_id, fresh=row.run_id is None)
    if lease is None:
        # A manual/rolling-version worker can already have settled an adopted run.
        close_finished(conn, row.run_id)
        return None
    if row.run_id is None:
        conn.execute(text("UPDATE acq_collection_plans SET run_id = :run WHERE id = :id"),
                     {"run": lease.run_id, "id": row.id})
    return lease, row.budget_seconds


def fingerprints_json(declared: dict[str, sources.DeclaredSource]) -> str:
    return json.dumps({key: sources.config_fingerprint(value) for key, value in declared.items()
                       if value.schedule is not None}, separators=(",", ":"))


def settle(conn: Connection, run_id: int, *, pending: bool) -> None:
    conn.execute(text("""
        UPDATE acq_collection_plans SET
            settled_at = CASE WHEN :pending THEN NULL ELSE clock_timestamp() END,
            not_before = CASE WHEN :pending THEN clock_timestamp() + :delay * interval '1 second' ELSE not_before END
        WHERE run_id = :run AND settled_at IS NULL
    """), {"run": run_id, "pending": pending, "delay": RETRY_DELAY_SECONDS})


def close_finished(conn: Connection, run_id: int | None) -> None:
    conn.execute(text("""
        UPDATE acq_collection_plans p SET settled_at = r.finished_at
        FROM acq_collection_runs r WHERE p.run_id = :run AND r.id = :run
          AND r.finished_at IS NOT NULL AND p.settled_at IS NULL
    """), {"run": run_id})
