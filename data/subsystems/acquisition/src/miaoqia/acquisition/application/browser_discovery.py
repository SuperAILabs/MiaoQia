"""Collect one rendered list page through the shared window and attempt ledger."""
from __future__ import annotations

import hashlib
import json
from dataclasses import replace
from datetime import date, timedelta
from urllib.parse import urlsplit

from sqlalchemy import text

from ..api.browser_projection import MAX_SELECTED_HTML_BYTES
from ..api.handlers import BrowserListPlan, BrowserListSourceHandler, BrowserSelection, FetchRequest, ListPage, SourceBlocked, Window
from ..infrastructure.archive_spool import Capture, SpoolFull
from ..infrastructure.browser_capture import BrowserCaptureError
from ..infrastructure.transport import MAX_BODY, TransportError, request_origin
from .browser_jobs import _BrowserRequestFailure, _render
from .discovery import ListPlanError
from .fetch import _ArchiveDeferred, _Failure, _Receipt, _attempt_url
from .sources import config_fingerprint


def plan_for(handler: BrowserListSourceHandler, channel: str, window: Window, page: int,
             previous_origin: tuple[str, str, int] | None) -> tuple[BrowserListPlan, tuple[str, str, int]]:
    try:
        plan = handler.browser_list(channel, window, page)
        if not isinstance(plan, BrowserListPlan) or not plan.body_selector:
            raise ValueError("missing rendered list plan or selector")
        origin = request_origin(plan.navigation_url)
        if (plan.navigation_url == plan.body_request.url or request_origin(plan.body_request.url) != origin
                or previous_origin not in (None, origin)):
            raise ListPlanError("target_outside_channel", plan.body_request.url)
    except ListPlanError:
        raise
    except (ValueError, TypeError, KeyError, IndexError, TransportError) as exc:
        raise ListPlanError("invalid_browser_list_plan") from exc
    return plan, origin


def collect(fetcher, src, plan, run):
    channel, window, page = run.position
    assert fetcher.spool is not None
    nav = FetchRequest("GET", plan.navigation_url)
    request = plan.body_request
    fingerprint = request.fingerprint()
    plan_key = _digest(json.dumps([nav.fingerprint(), plan.body_selector, config_fingerprint(src)],
                                  ensure_ascii=True).encode())
    scope = [channel, window.since.isoformat(), window.until.isoformat(), page, plan_key]
    task = fetcher._request_task_key(src, run, ["list", *scope], fingerprint)
    with fetcher._write(run) as conn:
        conn.execute(text("""
            INSERT INTO acq_request_tasks (task_key, run_id, kind, scope, fingerprint)
            VALUES (:task, :run, 'list', CAST(:scope AS jsonb), :fingerprint)
            ON CONFLICT (task_key) DO NOTHING
        """), {"task": task, "run": run.id, "scope": json.dumps(scope), "fingerprint": fingerprint})
    retain = src.access.list_retention_days > 0
    recovered = _read_spool(fetcher, run, task, days=src.access.list_retention_days) if retain else None
    attempt = None
    try:
        if recovered is None:
            capture, selection, attempts = _render(fetcher, src, plan, run, task)
            attempt, received = attempts[request]
            response = capture.response
            if (capture.method != "GET" or response.url != request.url or response.status != 200
                    or response.retry_after is not None or response.redirect_to is not None
                    or (capture.frame_id, capture.loader_id) != (selection.frame_id, selection.loader_id)):
                raise BrowserCaptureError("list response and selection do not share the approved request")
            navigation_attempt, _ = attempts[nav]
            request_id = capture.request_id
            staged_response = replace(response, url=_attempt_url(request.url))
        else:
            attempt, received = recovered.attempt_id, recovered.received_at
            staged_response = recovered.response
            if staged_response.url != _attempt_url(request.url):
                raise BrowserCaptureError("staged list targets a different request")
            response = replace(staged_response, url=request.url)
            selection, navigation_attempt, request_id = _decode(recovered.projection)

        html = selection.html.encode("utf-8")
        ids = (selection.navigation_id, selection.frame_id, selection.loader_id, request_id)
        if (not 0 < len(html) <= MAX_SELECTED_HTML_BYTES or not 0 < len(response.body) <= MAX_BODY
                or any(not _identifier(value) for value in ids) or selection.navigation_id == request_id
                or type(navigation_attempt) is not int or not 0 < navigation_attempt < attempt):
            raise BrowserCaptureError("unbounded or inconsistent list capture")
        listing = src.handler.parse_browser_list(channel, response, selection)
        if (not isinstance(listing, ListPage) or listing.groups or type(listing.has_next) is not bool
                or (listing.reported_page is None and listing.offset is None)):
            raise ValueError("browser listing requires publisher pagination metadata")
        if listing.offset is not None and len(listing.refs) != listing.offset.row_count:
            raise ValueError("browser listing did not validate every publisher row")
        notices = set()
        for ref in listing.refs:
            key = ref.source_key
            if (not _identifier(key, max_length=512) or key.startswith(("/", "?", "#"))
                    or key == urlsplit(ref.source_url).path.lstrip("/") or type(ref.published) is not date
                    or key in notices):
                raise ValueError("browser listing requires distinct publisher identities and dates")
            if request_origin(ref.source_url) != run.origin:
                raise ValueError("browser listing detail lies outside its origin")
            notices.add(key)
        listing = listing.for_request(page, window=window if src.handler.windowed else None)
        binding = {"version": 1, "task_key": task, "navigation_attempt_id": navigation_attempt,
                   "request_fingerprint": fingerprint, "request_id": request_id,
                   "navigation_id": selection.navigation_id, "frame_id": selection.frame_id,
                   "loader_id": selection.loader_id, "media_type": response.media_type,
                   "size_bytes": len(response.body), "selection_hash": _digest(html)}
        spool_capture = None
        if retain and recovered is None:
            metadata = json.dumps([navigation_attempt, *ids], separators=(",", ":")).encode()
            spool_capture = Capture(staged_response, attempt, received, metadata + b"\n" + html)
        with fetcher._write(run) as conn:
            if not _bound_attempts(conn, run, task, nav, request, navigation_attempt, attempt):
                raise BrowserCaptureError("list attempts are not bound to the same task and generation")
            if spool_capture is not None:
                _stage(fetcher, task, spool_capture)
            fetcher._task_state(conn, task, "staged" if retain else "queued", attempt)
    except (BrowserCaptureError, SourceBlocked, ValueError, TypeError, AttributeError, KeyError, IndexError,
            TransportError, OSError) as exc:
        receipt = _Receipt(recovered.response, attempt, recovered.received_at, task, fingerprint) if recovered else None
        if receipt is not None:
            fetcher._reject_capture(run, receipt)
        else:
            with fetcher._write(run) as conn:
                fetcher._task_state(conn, task, "failed", attempt)
        if isinstance(exc, SourceBlocked):
            raise
        return exc.failure if isinstance(exc, _BrowserRequestFailure) else _Failure("invalid_browser_list", False)

    return _Receipt(response, attempt, received, task, fingerprint, recovered=recovered is not None), listing, binding


def _read_spool(fetcher, run, task, *, days):
    try:
        with fetcher._write(run):
            staged = fetcher.spool.get(task)
            if staged is not None and staged.received_at + timedelta(days=days) <= fetcher.clock.now():
                fetcher.spool.discard(task, staged.attempt_id)
                staged = None
            if staged is None:
                fetcher.spool.ensure_room()
            return staged
    except SpoolFull as exc:
        raise _ArchiveDeferred("archive_spool_full") from exc
    except OSError as exc:
        raise _ArchiveDeferred("archive_spool_unavailable") from exc


def _stage(fetcher, task, capture):
    try:
        fetcher.spool.put(task, capture)
    except FileExistsError:
        if fetcher.spool.get(task) != capture:
            raise _ArchiveDeferred("archive_spool_conflict") from None
    except SpoolFull as exc:
        raise _ArchiveDeferred("archive_spool_full") from exc
    except OSError as exc:
        raise _ArchiveDeferred("archive_spool_unavailable") from exc


def _bound_attempts(conn, run, task, nav, request, navigation_attempt, attempt):
    rows = conn.execute(text("""
        SELECT a.id, a.fingerprint, a.url, d.generation, d.id AS dispatch_id
        FROM acq_fetch_attempts a JOIN acq_download_starts d ON d.id = a.download_id
        WHERE a.id IN (:navigation, :attempt) AND a.run_id = :run AND a.source_id = :source
          AND d.run_id = a.run_id AND d.task_key = :task AND d.fingerprint = a.fingerprint
          AND a.method = 'GET' AND a.status = 200 AND a.outcome = 'ok'
        ORDER BY a.id
    """), {"navigation": navigation_attempt, "attempt": attempt, "run": run.id,
           "source": run.report.source_id, "task": task}).all()
    return (len(rows) == 2 and rows[0].generation == rows[1].generation
            and rows[0].dispatch_id < rows[1].dispatch_id
            and [(row.fingerprint, row.url) for row in rows]
            == [(req.fingerprint(), _attempt_url(req.url)) for req in (nav, request)])


def _decode(projection):
    if projection is None:
        raise BrowserCaptureError("staged list selection is missing")
    metadata, _, html = projection.partition(b"\n")
    navigation_attempt, navigation_id, frame_id, loader_id, request_id = json.loads(metadata)
    return BrowserSelection(html.decode("utf-8"), navigation_id, frame_id, loader_id), navigation_attempt, request_id


def _identifier(value, *, max_length=256):
    return (isinstance(value, str) and 0 < len(value) <= max_length and value == value.strip()
            and all(ord(char) >= 32 and ord(char) not in (127, 0x2028, 0x2029) for char in value))


def _digest(value):
    return f"sha256:{hashlib.sha256(value).hexdigest()}"
