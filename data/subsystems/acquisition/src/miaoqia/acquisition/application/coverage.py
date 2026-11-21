"""Read source admission and collection coverage from acquisition-owned state."""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import date, datetime, timedelta

from sqlalchemy import Connection, text

from miaoqia.kernel import db, reqctx

from ..api import NAME
from ..api.handlers import Window
from .sources import CatalogSource

HISTORY_DAYS = 32
RECEIPT_IDS = 50
RUNTIME_READER_ROLE = "mq_acq_coverage_ro"


@dataclass
class HistoricalScan:
    """The receipts and positive list endpoint of one channel's traversal."""

    pages: list[tuple[int, int]] = field(default_factory=list)
    end: str | None = None
    manifest_unverified: bool = False

    def as_report(self) -> dict:
        return {"pages": self.pages, "end": self.end, "manifest_unverified": self.manifest_unverified}


def report(catalog: dict[str, CatalogSource], database: db.Database | None, *, window: Window | None = None,
           operator: bool = False) -> list[dict]:
    if window is not None and (window.until - window.since).days >= HISTORY_DAYS:
        raise ValueError(f"coverage window cannot exceed {HISTORY_DAYS} days")
    channels = {sid: list(entry.declaration.handler.channels) if entry.declaration else []
                for sid, entry in catalog.items()}
    states = _runtime_states(database, channels, window, operator) if database is not None else {}
    return [
        {"source_id": entry.source_id, "readiness": entry.readiness, "access_mode": entry.access_mode,
         "admission": entry.admission, "readiness_gap": entry.readiness_gap,
         "handler": entry.declaration.handler_ref if entry.declaration else None,
         "transport": entry.declaration.transport if entry.declaration else None,
         "channels": channels[entry.source_id],
         "runtime": states.get(entry.source_id)}
        for entry in sorted(catalog.values(), key=lambda entry: entry.source_id)
    ]


def _require_runtime_reader(conn: Connection) -> None:
    allowed = conn.execute(text("""
        SELECT current_database() = 'acq' AND current_user = :role AND session_user = :role
          AND EXISTS (
              SELECT 1 FROM pg_catalog.pg_roles role
              WHERE role.rolname = :role AND role.rolcanlogin
                AND NOT (role.rolsuper OR role.rolcreatedb OR role.rolcreaterole OR role.rolreplication
                         OR role.rolbypassrls OR role.rolinherit)
                AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members member WHERE member.member = role.oid)
          )
    """), {"role": RUNTIME_READER_ROLE}).scalar_one()
    if not allowed:
        raise ValueError("unexpected coverage database identity")
    conn.execute(text("SET LOCAL search_path TO pg_catalog, acq_coverage, public"))


def _runtime_states(database: db.Database, channels: dict[str, list[str]], window: Window | None,
                    operator: bool) -> dict[str, dict]:
    with reqctx.use(reqctx.service(NAME)), database.tx() as conn:
        conn.execute(text("SET TRANSACTION READ ONLY"))
        if operator:
            _require_runtime_reader(conn)
        rows = conn.execute(text("""
            SELECT s.source_id, s.state, s.state_reason AS blocked_reason,
                   h.state AS health, h.reason_code, h.checked_at, h.last_success_at,
                   latest.id AS incremental_run, latest.window_start, latest.window_end,
                   latest.started_at, latest.finished_at, latest.incremental_report,
                   coalesce(pending.total, 0) AS pending_incremental_count,
                   coalesce(pending.windows, '[]'::jsonb) AS pending_incremental_windows,
                   timing.next_due_at,
                   coalesce(queue.online, 0) AS pending_online_plans,
                   coalesce(queue.historical, 0) AS pending_historical_plans,
                   coalesce(tasks.counts, '{}'::jsonb) AS incremental_request_tasks,
                   b.oldest_day AS initial_backfill_start, b.next_day AS initial_backfill_pending,
                   coalesce(backfill.gaps, '[]'::jsonb) AS initial_backfill_gaps,
                   coalesce(history.runs, '[]'::jsonb) AS historical_runs
            FROM acq_sources s
            LEFT JOIN acq_source_health h USING (source_id)
            LEFT JOIN acq_initial_backfill b USING (source_id)
            LEFT JOIN acq_collection_schedule timing USING (source_id)
            LEFT JOIN LATERAL (
                SELECT count(*) FILTER (WHERE mode = 'incremental') AS online,
                       count(*) FILTER (WHERE mode = 'backfill') AS historical
                FROM acq_collection_plans WHERE source_id = s.source_id AND settled_at IS NULL
            ) queue ON true
            LEFT JOIN LATERAL (
                SELECT jsonb_agg(jsonb_build_object('day', g.day, 'run_id', g.last_run_id,
                    'wait_for_source_change', coalesce(r.report->>'wait_for_source_change' = 'true', false)
                ) ORDER BY g.day DESC) AS gaps
                FROM acq_initial_backfill_gaps g JOIN acq_collection_runs r ON r.id = g.last_run_id
                WHERE g.source_id = s.source_id
            ) backfill ON true
            LEFT JOIN LATERAL (
                SELECT jsonb_agg(jsonb_build_object('day', window_start, 'run_id', id,
                    'mode', mode, 'run_finished', finished_at IS NOT NULL, 'generation', generation,
                    'max_pages', max_pages, 'scan', coalesce(report, pending_report)->'historical_scan'
                ) ORDER BY window_start DESC) AS runs
                FROM (
                    SELECT DISTINCT ON (window_start) id, window_start, mode, finished_at, generation,
                           max_pages, report, pending_report
                    FROM acq_collection_runs
                    WHERE source_id = s.source_id AND window_start = window_end
                      AND (CAST(:since AS date) IS NULL OR window_start BETWEEN :since AND :until)
                      AND (mode = 'backfill' OR (mode = 'manual'
                           AND window_start < (started_at AT TIME ZONE 'Asia/Shanghai')::date))
                    ORDER BY window_start DESC, id DESC LIMIT :history_days
                ) recent
            ) history ON true
            LEFT JOIN LATERAL (
                SELECT id, window_start, window_end, started_at, finished_at,
                       coalesce(report, pending_report) AS incremental_report
                FROM acq_collection_runs WHERE source_id = s.source_id AND mode = 'incremental'
                ORDER BY id DESC LIMIT 1
            ) latest ON true
            LEFT JOIN LATERAL (
                SELECT max(total) AS total, jsonb_agg(jsonb_build_object(
                    'run_id', id, 'since', window_start, 'until', window_end, 'generation', generation
                ) ORDER BY generation, id) AS windows
                FROM (
                    SELECT id, window_start, window_end, generation, count(*) OVER () AS total
                    FROM acq_collection_runs
                    WHERE source_id = s.source_id AND mode = 'incremental' AND finished_at IS NULL AND generation > 0
                    ORDER BY generation, id LIMIT 32
                ) older
            ) pending ON true
            LEFT JOIN LATERAL (
                SELECT jsonb_object_agg(state, total) AS counts
                FROM (
                    SELECT state, count(*) AS total FROM acq_request_tasks
                    WHERE run_id = latest.id GROUP BY state
                ) per_state
            ) tasks ON true
        """), {"history_days": HISTORY_DAYS, "since": window.since if window else None,
               "until": window.until if window else None}).mappings().all()
        states = {}
        for row in rows:
            sid = row["source_id"]
            if sid in channels:
                states[sid] = {key: _value(value) for key, value in row.items()
                               if key not in ("source_id", "historical_runs")}
                history = _historical_coverage(conn, sid, channels[sid], row["historical_runs"])
                states[sid]["historical_coverage"] = _include_unknown_days(history, channels[sid], window)
        return states


def _include_unknown_days(history: list[dict], channels: list[str], window: Window | None) -> list[dict]:
    if window is None:
        return history
    recorded = {(row["day"], row["channel"]): row for row in history}
    rows = []
    for offset in range((window.until - window.since).days + 1):
        day = (window.until - timedelta(days=offset)).isoformat()
        for channel in channels:
            row = recorded.get((day, channel))
            if row is None:
                row = {"day": day, "channel": channel, "run_id": None, "mode": None, "run_finished": False,
                       "list_status": "unknown", "required_originals_status": "unknown",
                       "list_receipt_count": 0, "list_receipt_attempt_ids": []}
            rows.append(row)
    return rows


def _scan_pages(run: dict, channel: str) -> dict | None:
    checkpoint = run["scan"]
    if (not isinstance(checkpoint, dict) or type(checkpoint.get("generation")) is not int
            or checkpoint["generation"] != run["generation"] or not isinstance(checkpoint.get("channels"), dict)):
        return None
    scan = checkpoint["channels"].get(channel)
    if (not isinstance(scan, dict) or "end" not in scan or scan["end"] not in (None, "terminal", "ordered_cutoff")
            or type(scan.get("manifest_unverified")) is not bool or not isinstance(scan.get("pages"), list)
            or len(scan["pages"]) > run["max_pages"]):
        return None
    pages = scan["pages"]
    if any(not isinstance(page, list) or len(page) != 2 or any(type(n) is not int for n in page)
           or not 0 < page[0] < 2**63 or page[1] < 0 for page in pages):
        return None
    if len({page[0] for page in pages}) != len(pages) or (scan["end"] and not pages):
        return None
    return scan


def unverified_channels(conn: Connection, source_id: str, channels: tuple[str, ...], run_id: int) -> list[str]:
    """Check a just-finished day against the same ledger evidence used by the operator report."""
    run = conn.execute(text("""
        SELECT to_char(window_start, 'YYYY-MM-DD') AS day, id AS run_id, mode, true AS run_finished,
               generation, max_pages, report->'historical_scan' AS scan
        FROM acq_collection_runs WHERE id = :id AND source_id = :source
          AND window_start = window_end AND finished_at IS NOT NULL
    """), {"id": run_id, "source": source_id}).mappings().one()
    return [row["channel"] for row in _historical_coverage(conn, source_id, list(channels), [run])
            if (row["list_status"], row["required_originals_status"])
            not in {("complete", "archived"), ("proven_empty", "not_required")}]


def _historical_coverage(conn: Connection, source_id: str, channels: list[str], runs: list[dict]) -> list[dict]:
    scans = [(run, channel, _scan_pages(run, channel)) for run in runs for channel in channels]
    ids = {page[0] for _, _, scan in scans if scan is not None for page in scan["pages"]}
    receipts = {}
    groups = {}
    if ids:
        receipts = {row.attempt_id: row for row in conn.execute(text("""
            SELECT p.attempt_id, p.run_id, p.channel, p.window_start, p.window_end, p.page, p.refs, p.has_next
            FROM acq_discovery_pages p JOIN acq_fetch_attempts a ON a.id = p.attempt_id AND a.run_id = p.run_id
            JOIN acq_download_starts d ON d.id = a.download_id AND d.run_id = p.run_id
            JOIN acq_request_tasks t ON t.task_key = d.task_key AND t.run_id = p.run_id AND t.kind = 'list'
                AND t.scope->>0 = p.channel AND t.scope->>1 = to_char(p.window_start, 'YYYY-MM-DD')
                AND t.scope->>2 = to_char(p.window_end, 'YYYY-MM-DD') AND t.scope->3 = to_jsonb(p.page)
            WHERE p.attempt_id = ANY(:ids) AND a.source_id = :source AND a.status = 200 AND a.outcome = 'ok'
        """), {"source": source_id, "ids": list(ids)})}
        if any("group_key" in ref for page in receipts.values() for ref in page.refs if isinstance(ref, dict)):
            groups = _group_receipts(conn, source_id, list({run["run_id"] for run in runs}))
    results = []
    key_sets = []
    for run, channel, scan in scans:
        status, keys, missing_group, unresolved, attempt_ids = _listing(run, channel, scan, receipts, groups)
        results.append({
            **{key: run[key] for key in ("day", "run_id", "mode", "run_finished")},
            "channel": channel, "list_status": status, "required_originals_status": "unknown",
            "list_receipt_count": len(attempt_ids), "list_receipt_attempt_ids": attempt_ids[:RECEIPT_IDS],
        })
        key_sets.append((keys, missing_group, unresolved or (scan is not None and scan["manifest_unverified"])))
    originals = original_statuses(conn, source_id, {key for keys, _, _ in key_sets for key in keys})
    for result, (keys, missing_group, unresolved) in zip(results, key_sets, strict=True):
        notice_statuses = {originals.get((key, result["day"]), "missing") for key in keys}
        if result["list_status"] == "proven_empty":
            result["required_originals_status"] = "not_required"
        elif missing_group or "missing" in notice_statuses:
            result["required_originals_status"] = "missing"
        elif result["list_status"] == "complete" and not unresolved and "unknown" not in notice_statuses:
            result["required_originals_status"] = "archived"
    return results


def _listing(run, channel, scan, receipts, groups):
    unknown = ("unknown", set(), False, True, [])
    if scan is None:
        return unknown
    day = run["day"]
    keys = set()
    matching_group_members = False
    missing_group = False
    undated = False
    unresolved = False
    attempt_ids = []
    previous_day = None
    for number, (attempt, count) in enumerate(scan["pages"], 1):
        page = receipts.get(attempt)
        if (page is None or page.run_id != run["run_id"] or page.channel != channel
                or page.window_start.isoformat() != day or page.window_end != page.window_start
                or page.page != number or len(page.refs) != count):
            return unknown
        if scan["end"] and page.has_next != (number < len(scan["pages"]) or scan["end"] == "ordered_cutoff"):
            return unknown
        attempt_ids.append(attempt)
        for ref in page.refs:
            if not isinstance(ref, dict):
                return unknown
            if "group_key" in ref:
                if not isinstance(ref["group_key"], str) or not isinstance(ref.get("request_fingerprint"), str):
                    return unknown
                members = groups.get((run["run_id"], channel, day, ref["group_key"], ref["request_fingerprint"]))
                if members is None:
                    missing_group = True
                    continue
                for member in members:
                    if not isinstance(member, dict) or not _iso_day(member.get("published_on")):
                        return unknown
                    in_window = member["published_on"] == day
                    if (in_window and member.get("outcome") not in ("archived", "unchanged", "stale")
                            or not in_window and member.get("outcome") != "outside_window"):
                        return unknown
                    matching_group_members |= in_window
                    unresolved |= in_window and member["outcome"] == "stale"
                continue
            if ("notice_key" not in ref or "published_on" not in ref
                    or (ref["notice_key"] is not None and
                        (not isinstance(ref["notice_key"], str) or not ref["notice_key"]))):
                return unknown
            published = ref["published_on"]
            if published is None:
                undated = True
            elif not _iso_day(published):
                return unknown
            else:
                if scan["end"] == "ordered_cutoff" and previous_day is not None and published > previous_day:
                    return unknown
                previous_day = published
                if published == day:
                    if ref["notice_key"] is None:
                        unresolved = True
                    else:
                        keys.add(ref["notice_key"])
    if scan["end"] == "ordered_cutoff" and (previous_day is None or previous_day >= day):
        return unknown
    status = "incomplete"
    if scan["end"] and not (missing_group or undated):
        status = "complete" if keys or unresolved or matching_group_members else "proven_empty"
    return status, keys, missing_group, unresolved, attempt_ids


def _iso_day(value) -> bool:
    try:
        return isinstance(value, str) and date.fromisoformat(value).isoformat() == value
    except ValueError:
        return False


def _group_receipts(conn: Connection, source_id: str, run_ids: list[int]) -> dict:
    # On same-run recovery the settled task may still point at an earlier receipt for the same group.
    return {(row.run_id, row.channel, row.window_start.isoformat(), row.group_key, row.fingerprint): row.members
            for row in conn.execute(text("""
                SELECT g.run_id, g.channel, g.window_start, g.group_key, t.fingerprint, g.members
                FROM acq_group_receipts g
                JOIN acq_request_tasks t ON t.task_key = g.task_key AND t.run_id = g.run_id
                    AND t.kind = 'group' AND t.state = 'settled' AND t.last_attempt = g.attempt_id
                JOIN acq_discovery_pages p ON p.attempt_id = g.list_attempt_id AND p.run_id = g.run_id
                    AND p.channel = g.channel AND p.window_start = g.window_start AND p.window_end = g.window_end
                    AND t.scope->4 = to_jsonb(p.attempt_id)
                    AND p.refs @> jsonb_build_array(jsonb_build_object('group_key', g.group_key,
                                                                     'request_fingerprint', t.fingerprint))
                WHERE g.source_id = :source AND g.run_id = ANY(:runs) AND g.window_start = g.window_end
            """), {"source": source_id, "runs": run_ids})}


def original_statuses(conn: Connection, source_id: str, keys: set[str]) -> dict[tuple[str, str], str]:
    """Verify existing detail and attachment coverage for identified announcement dates."""
    if not keys:
        return {}
    return {(row.notice_key, row.published_on.isoformat()): row.status for row in conn.execute(text("""
        SELECT o.notice_key, o.published_on,
            CASE
                WHEN EXISTS (
                    SELECT 1 FROM acq_observation_attachments p
                    WHERE (p.source_id, p.notice_key, p.version) = (o.source_id, o.notice_key, o.version)
                      AND p.required AND p.status <> 'available'
                ) THEN 'missing'
                WHEN jsonb_typeof(result.summary->'failures') IS DISTINCT FROM 'array'
                    OR result.summary->'historical_scan'->'channels'->o.channel->'manifest_unverified' = 'true'::jsonb
                    OR EXISTS (
                        SELECT 1 FROM jsonb_array_elements(
                            CASE WHEN jsonb_typeof(result.summary->'failures') = 'array'
                                 THEN result.summary->'failures' ELSE '[]'::jsonb END
                        ) AS issue
                        WHERE issue->>'notice_key' = o.notice_key AND issue->>'part_key' IS NULL
                    ) THEN 'unknown'
                ELSE 'archived'
            END AS status
        FROM acq_detail_checks c JOIN acq_source_observations o USING (source_id, notice_key, version)
        JOIN acq_raw_artifacts a ON a.source_id = o.source_id AND a.id = o.artifact_id
        JOIN acq_fetch_attempts attempt ON attempt.id = c.attempt_id AND attempt.source_id = c.source_id
        LEFT JOIN acq_collection_runs r ON r.id = attempt.run_id AND r.source_id = c.source_id
        CROSS JOIN LATERAL (SELECT coalesce(r.report, r.pending_report) AS summary) result
        WHERE c.source_id = :source AND c.notice_key = ANY(:keys)
          AND o.published_on IS NOT NULL AND o.selector IS NULL
    """), {"source": source_id, "keys": list(keys)})}


def _value(value):
    return value.isoformat() if isinstance(value, (date, datetime)) else value
