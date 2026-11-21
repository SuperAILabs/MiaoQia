"""One publisher-approved browser body request on the acquisition task ledger."""
from __future__ import annotations

import json
from collections.abc import Callable, Iterator
from contextlib import AbstractContextManager, contextmanager
from datetime import date
from typing import Protocol

from sqlalchemy import text

from miaoqia.kernel.objectstore import ObjectConflict

from ..api.handlers import BrowserDetailPlan, BrowserPagePlan, BrowserSelection, DetailRef, FetchRequest, SourceBlocked, Window
from ..infrastructure.browser_capture import BrowserCapture, BrowserCaptureError
from ..infrastructure.transport import TransportError, request_origin
from .browser_observations import BrowserConflict, archive_browser_pair, prepare_browser_pair
from .fetch import _ArchiveDeferred, _DeadlineReached, _Failure, _retry_after

Complete = Callable[[int, str | None], None]
Permit = Callable[[FetchRequest], AbstractContextManager[Complete]]


class BrowserRenderer(Protocol):
    def render(self, plan: BrowserPagePlan, permit: Permit, *, timeout: float) -> tuple[BrowserCapture, BrowserSelection]:
        """Use a confined page. Hold a permit across every actual request and observe its final status.

        The caller's permit is in addition to the renderer's independent egress enforcement.
        For this first slice only the navigation and distinct body GET may reach the publisher.
        """


class _BrowserRequestFailure(BrowserCaptureError):
    def __init__(self, failure: _Failure):
        super().__init__(failure.code)
        self.failure = failure


def _render(fetcher, src, plan, run, task_key):
    assert fetcher.browser is not None
    navigation = FetchRequest("GET", plan.navigation_url)
    body = plan.body_request
    allowed = (navigation, body)
    started_requests = set()
    attempts = {}
    denied = False
    deadline = min(run.deadline, fetcher.clock.monotonic() + src.access.timeout_seconds)

    @contextmanager
    def permit(request: FetchRequest) -> Iterator[Complete]:
        nonlocal denied
        if request not in allowed or request in started_requests or (request == body and navigation not in attempts):
            denied = True
            raise BrowserCaptureError("browser request is outside the verified plan")
        started_requests.add(request)
        with fetcher._admit_hop(src, request, run, task_key, capture=False, deadline=deadline) as (slot, download_id, _):
            started = fetcher.clock.monotonic()
            status = None
            retry_after = None

            def complete(value: int, retry: str | None) -> None:
                nonlocal denied, status, retry_after
                if (status is not None or type(value) is not int or not 100 <= value <= 599
                        or (retry is not None and (not isinstance(retry, str) or len(retry) > 256))):
                    denied = True
                    raise BrowserCaptureError("invalid browser attempt receipt")
                status, retry_after = value, retry

            try:
                yield complete
            finally:
                outcome = "ok"
                failure = None
                if status is None:
                    outcome = "network_error"
                elif status in (401, 403):
                    outcome = "blocked"
                elif status == 429 or retry_after is not None:
                    outcome = "throttled"
                    failure = _Failure("throttled", True)
                    delay = max(src.access.throttle_backoff_seconds, _retry_after(retry_after, fetcher.clock.now()))
                    slot.cooldown(delay, shared_site=True)
                elif status != 200:
                    outcome = "http_error"
                    failure = _Failure(f"http_{status}", status >= 500)
                attempt = fetcher._attempt(slot.connection, run, request, download_id, status,
                                           outcome, started, None)
                if outcome == "ok":
                    attempts[request] = (attempt, fetcher.clock.now())
                elif outcome == "blocked":
                    raise SourceBlocked(f"http_{status}")
                elif failure is not None:
                    raise _BrowserRequestFailure(failure)

    # Refuse partial and unreachable artifacts even if an adapter catches a rejected permit.
    try:
        capture, selected = fetcher.browser.render(plan, permit, timeout=deadline - fetcher.clock.monotonic())
    except _DeadlineReached as exc:
        fetcher._require_time(run)
        raise _BrowserRequestFailure(_Failure("browser_deadline", True)) from exc
    fetcher._require_time(run)
    if fetcher.clock.monotonic() >= deadline:
        raise _BrowserRequestFailure(_Failure("browser_deadline", True))
    if denied or set(attempts) != set(allowed):
        raise BrowserCaptureError("browser capture did not exercise the complete request plan")
    return capture, selected, attempts


def collect(fetcher, src, channel: str, ref: DetailRef, list_attempt_id: int, run) -> None:
    assert run.position is not None and fetcher.spool is not None
    window = run.position[1]
    if ref.source_key is None:
        run.report.pending_identity += 1
        return
    try:
        plan = src.handler.browser_detail(channel, ref)
        if (not isinstance(plan, BrowserDetailPlan) or plan.navigation_url != ref.source_url
                or plan.body_request.url == plan.navigation_url
                or request_origin(plan.navigation_url) != run.origin or request_origin(plan.body_request.url) != run.origin):
            raise ValueError("invalid browser navigation plan")
    except (ValueError, TransportError):
        fetcher._failed(src, ref.source_url, _Failure("invalid_browser_plan", False), run)
        return

    scope = [channel, window.since.isoformat(), window.until.isoformat(), ref.source_key, list_attempt_id]
    fingerprint = plan.body_request.fingerprint()
    task = fetcher._request_task_key(src, run, ["browser", *scope], fingerprint)
    with fetcher._write(run) as conn:
        conn.execute(text("""
            INSERT INTO acq_request_tasks (task_key, run_id, kind, scope, fingerprint)
            VALUES (:task, :run, 'browser', CAST(:scope AS jsonb), :fingerprint)
        """), {"task": task, "run": run.id, "scope": json.dumps(scope), "fingerprint": fingerprint})
    try:
        capture, selection, attempts = _render(fetcher, src, plan, run, task)
        attempt, received = attempts[plan.body_request]
        checkpoint, projection = prepare_browser_pair(
            lease=run.lease, ref=ref, request=plan.body_request, capture=capture, selection=selection,
            validate=src.handler.validate_browser_notice, task_key=task, attempt_id=attempt,
            list_attempt_id=list_attempt_id, received_at=received,
        )
        published = date.fromisoformat(checkpoint["published_on"])
        with fetcher._write(run) as conn:
            if not window.since <= published <= window.until:
                fetcher._task_state(conn, task, "settled", attempt)
                run.report.skipped += 1
                return
            fetcher._task_state(conn, task, "queued", attempt)
    except (BrowserCaptureError, ValueError, TransportError, OSError, SourceBlocked) as exc:
        with fetcher._write(run) as conn:
            fetcher._task_state(conn, task, "failed")
        if isinstance(exc, SourceBlocked):
            raise
        failure = exc.failure if isinstance(exc, _BrowserRequestFailure) else _Failure("invalid_browser_capture", False)
        fetcher._failed(src, ref.source_url, failure, run)
        return
    try:
        _archive(fetcher, channel, window, checkpoint, run, captured=(capture.response.body, projection))
    except BrowserConflict:
        with fetcher._write(run) as conn:
            fetcher._task_state(conn, task, "failed", attempt)
        fetcher._failed(src, ref.source_url, _Failure("browser_observation_conflict", False), run)


def _archive(fetcher, channel, window, checkpoint, run, *, captured=None):
    try:
        result = archive_browser_pair(fetcher.db, fetcher.store, fetcher.spool, lease=run.lease,
                                      channel=channel, window=window, checkpoint=checkpoint, captured=captured)
    except FileNotFoundError as exc:
        if captured is None:
            raise
        raise _ArchiveDeferred("browser_archive_pending") from exc
    except (OSError, ObjectConflict) as exc:
        raise _ArchiveDeferred("browser_archive_pending") from exc
    run.checked.add(checkpoint["notice_key"])
    run.report.archived += int(result.changed and not result.replayed)
    run.report.unchanged += int(not result.changed and not result.replayed)


def recover(fetcher, src, run) -> None:
    with fetcher.db.tx() as conn:
        scopes = conn.execute(text("""
            SELECT t.scope, d.page FROM acq_request_tasks t
            JOIN acq_discovery_pages d ON d.run_id = t.run_id AND to_jsonb(d.attempt_id) = t.scope->4
            WHERE t.run_id = :run AND t.kind = 'browser' AND t.state = 'staged'
            ORDER BY t.updated_at, t.task_key
        """), {"run": run.id}).all()
    for scope, page in scopes:
        channel, since, until, _, _, checkpoint = scope
        fetcher._require_time(run)
        if channel not in src.handler.channels:
            raise _ArchiveDeferred("browser_recovery_contract_changed")
        window = Window(date.fromisoformat(since), date.fromisoformat(until))
        run.position = (channel, window, page)
        try:
            _archive(fetcher, channel, window, checkpoint, run)
        except (BrowserConflict, FileNotFoundError):
            binding = checkpoint["binding"]
            with fetcher._write(run) as conn:
                fetcher._task_state(conn, binding["task_key"], "failed", binding["attempt_id"])
            fetcher._gap(run, channel, window, page, "browser_archive_recapture")
