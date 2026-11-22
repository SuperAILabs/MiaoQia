"""Fetch runtime: list discovery, detail fetch, rate limiting, retry and backoff, dedup, archive and outbox events.

One run collects one source over a publication window. Every request goes through shared source and site budgets and is
recorded in acq_fetch_attempts. Missing-only runs skip known notices. A recheck compares the bytes with the
latest observation, allowing reversions to refer to an artifact already stored. A source that answers with a captcha,
login wall or 401/403 is marked blocked and stays stopped until an operator unblocks it; the runtime never tries to
get around it (acquisition.md section 9).

Object store writes precede the database transaction: objects are immutable and content-addressed, so a rolled back
transaction leaves only an unreferenced object.
"""
from __future__ import annotations

import hashlib
import json
import logging
import time
from collections.abc import Iterator
from contextlib import ExitStack, contextmanager
from dataclasses import dataclass, field, replace
from datetime import date, datetime, timedelta, timezone
from email.utils import parsedate_to_datetime
from typing import TYPE_CHECKING, Protocol
from urllib.parse import urlsplit

from sqlalchemy import Connection, text

from miaoqia.kernel import db, outbox, reqctx
from miaoqia.kernel.objectstore import ObjectConflict, ObjectStore

from ..api import NAME
from ..api.handlers import (DetailFetch, DetailRef, FetchRequest, FetchResponse, ListPage, NoticeGroup, SourceBlocked, Window,
                            checked_detail_ref, resolve_detail_request)
from ..infrastructure.archive_spool import ArchiveSpool, Capture, IncomingCapture, InvalidCaptureMetadata, SpoolFull, SpoolWriteError
from ..infrastructure.download_slots import DownloadSlots
from ..infrastructure.transport import MAX_BODY, REDIRECTS, TransportError, redirect_request, request_origin, site_identity
from . import collection_leases
from .collection_leases import CollectionLease, LeaseLost
from . import discovery, initial_backfill, plans, sources
from .coverage import HistoricalScan, original_statuses
from .sources import DeclaredSource

if TYPE_CHECKING:
    from .browser_jobs import BrowserRenderer

NOTICE_ARCHIVED = "acquisition.notice.archived.v1"
FAILED = "acquisition.fetch.failed.v1"
HEALTH_CHANGED = "acquisition.source.health_changed.v1"
CHINA = timezone(timedelta(hours=8))
MANUAL_BUDGET_SECONDS = 900
MAX_REDIRECTS = 5
LEASE_WAIT_STEP_SECONDS = 60
MAX_ATTACHMENTS = 16
MAX_RECOVERY_NOTICES = 8
log = logging.getLogger(__name__)


def china_today() -> date:
    """Publication dates on the pilot sources are China dates."""
    return datetime.now(CHINA).date()

writer = outbox.Writer(subsystem=NAME, namespace=NAME, zone=reqctx.PUBLIC)


def rights_tags(source_id: str) -> list[str]:
    """Provenance tag carried by every artifact of a public source; current rights are resolved downstream."""
    return [f"public:{source_id}"]


class Transport(Protocol):
    def send(self, request: FetchRequest, timeout: float) -> FetchResponse: ...


class Clock(Protocol):
    def monotonic(self) -> float: ...

    def sleep(self, seconds: float) -> None: ...

    def now(self) -> datetime: ...


class SystemClock:
    def monotonic(self) -> float:
        return time.monotonic()

    def sleep(self, seconds: float) -> None:
        time.sleep(seconds)

    def now(self) -> datetime:
        return datetime.now(timezone.utc)


class SourceStopped(RuntimeError):
    """The source is blocked; an operator must review and unblock it."""


class _DeadlineReached(RuntimeError):
    pass


class _RightsRevoked(RuntimeError):
    pass


class _ArchiveDeferred(RuntimeError):
    pass


@dataclass
class RunReport:
    source_id: str
    pages: int = 0
    listed: int = 0
    archived: int = 0
    unchanged: int = 0
    pending_identity: int = 0
    skipped: int = 0
    failed: int = 0
    blocked: str | None = None
    truncated: bool = False                      # the run reached its deadline before finishing
    health: str | None = None
    failures: list[dict] = field(default_factory=list)
    run_id: int | None = None
    gaps: list[dict] = field(default_factory=list)
    deferred: str | None = None


@dataclass
class _Run:
    lease: CollectionLease
    deadline: float
    report: RunReport
    last_url: str = ""
    position: tuple[str, Window, int] | None = None
    checked: set[str] = field(default_factory=set)
    origin: tuple[str, str, int] | None = None
    attempted: set[str] = field(default_factory=set)
    attachment: tuple[str, int, str] | None = None
    historical_scans: dict[str, HistoricalScan] = field(default_factory=dict)

    @property
    def id(self) -> int:
        return self.lease.run_id


@dataclass(frozen=True)
class _Failure:
    code: str
    retryable: bool


@dataclass(frozen=True)
class _Receipt:
    response: FetchResponse
    attempt_id: int
    received_at: datetime
    task_key: str
    request_fingerprint: str
    recovered: bool = False
    streamed: bool = False


@dataclass(frozen=True)
class _Attachment:
    part_key: str
    required: bool
    name: str | None
    request_fingerprint: str | None
    artifact_id: int | None = None
    attempt_id: int | None = None
    artifact_ref: str | None = None
    content_hash: str | None = None
    media_type: str | None = None
    size_bytes: int | None = None

    def event(self) -> dict:
        result = {"part_key": self.part_key, "required": self.required,
                  "status": "available" if self.artifact_id is not None else "pending"}
        if self.name is not None:
            result["name"] = self.name
        if self.artifact_id is not None:
            result.update(artifact_ref=self.artifact_ref, content_hash=self.content_hash,
                          media_type=self.media_type, size_bytes=self.size_bytes)
        return result


class Fetcher:
    def __init__(self, database: db.Database, store: ObjectStore, transport: Transport, *, clock: Clock | None = None,
                 spool: ArchiveSpool | None = None, browser: BrowserRenderer | None = None):
        self.db = database
        self.store = store
        self.transport = transport
        self._stream = getattr(transport, "send_capture", None)
        self.spool = spool
        self.browser = browser
        self.clock = clock or SystemClock()
        if clock is None:
            self.slots = DownloadSlots(database)
        else:
            self.slots = DownloadSlots(database, now=lambda: datetime.fromtimestamp(clock.monotonic(), timezone.utc))

    # ---- registry -------------------------------------------------------------------------------------------
    register = staticmethod(sources.register)

    # ---- run ------------------------------------------------------------------------------------------------
    def run(self, src: DeclaredSource, window: Window, *, max_pages: int, refetch: bool = False,
            unblock: bool = False, deadline: float | None = None, mode: str = "manual",
            recover: bool = False, resume_id: int | None = None) -> RunReport:
        """Collect one window or resume a specified open run with its original plan and a new generation."""
        if max_pages < 1:
            raise ValueError("max_pages must be positive")
        if deadline is None:
            deadline = self.clock.monotonic() + MANUAL_BUDGET_SECONDS
        with reqctx.use(reqctx.service(NAME)):
            with self.db.tx() as conn:
                state = self.register(conn, src)
                if state == "blocked" and not unblock:
                    raise SourceStopped(src.source_id)
                lease = collection_leases.claim(conn, src.source_id, window, max_pages=max_pages, refetch=refetch,
                                                mode=mode, started_at=self.clock.now(), recover=recover,
                                                resume_id=resume_id)
                if lease is None:
                    reason = "source_busy" if resume_id is None else "recovery_not_claimed"
                    return RunReport(src.source_id, deferred=reason)
                if state == "blocked":
                    conn.execute(text("UPDATE acq_sources SET state = 'active', state_reason = NULL, updated_at = now() "
                                      "WHERE source_id = :s"), {"s": src.source_id})
        return self.execute_claimed(src, lease, deadline=deadline)

    def execute_claimed(self, src: DeclaredSource, lease: CollectionLease, *, deadline: float) -> RunReport:
        """Execute a window whose source lease has already been committed."""
        if src.source_id != lease.source_id:
            raise ValueError("source does not match the collection lease")
        with reqctx.use(reqctx.service(NAME)):
            report = RunReport(src.source_id, run_id=lease.run_id)
            run = _Run(lease, deadline, report)
            rendered = src.transport == "browser"
            try:
                try:
                    if rendered:
                        from . import browser_jobs

                        if self.browser is None or self.spool is None:
                            raise _ArchiveDeferred("browser_runtime_unavailable")
                        browser_jobs.recover(self, src, run)
                    if callable(getattr(src.handler, "group_request", None)):
                        self._drain_groups(src, run)
                    for channel in src.handler.channels:
                        for w in self._windows(src, lease.window):
                            self._channel(src, channel, w, lease.max_pages, lease.refetch, run)
                    if lease.mode == "incremental" and not rendered:
                        self._recover_attachments(src, run)
                except SourceBlocked as blocked:
                    self._block(src, blocked.code, run)
            except _DeadlineReached:
                self._interrupt(run, "deadline")
            except LeaseLost:
                self._interrupt(run, "lease_lost")
                return report
            except _RightsRevoked:
                report.blocked = "source_rights_revoked"
                if run.position is not None:
                    self._gap(run, *run.position, report.blocked)
            except _ArchiveDeferred as deferred:
                report.deferred = str(deferred)
                if run.position is not None:
                    self._gap(run, *run.position, report.deferred)
            try:
                self._health(src, run)
            except LeaseLost:
                self._interrupt(run, "lease_lost")
            return report

    def _remaining(self, run: _Run) -> float:
        return run.deadline - self.clock.monotonic()

    def _require_time(self, run: _Run) -> None:
        if self._remaining(run) <= 0:
            raise _DeadlineReached

    def _interrupt(self, run: _Run, reason: str) -> None:
        run.report.truncated = True
        if run.position is not None:
            self._gap(run, *run.position, reason)

    @contextmanager
    def _write(self, run: _Run) -> Iterator[Connection]:
        self._require_time(run)
        with run.lease.writable(self.db) as conn:
            self._require_time(run)
            self._check_rights(conn, run)
            yield conn
            self._require_time(run)

    def _gap(self, run: _Run, channel: str, window: Window, page: int, reason: str) -> None:
        run.report.gaps.append({"channel": channel, "since": window.since.isoformat(),
                                "until": window.until.isoformat(), "page": page, "reason": reason})

    @staticmethod
    def _request_task_key(src, run, logical, fingerprint):
        return hashlib.sha256(json.dumps([src.source_id, run.id, src.handler_ref, logical, fingerprint],
                                        separators=(",", ":"), ensure_ascii=False).encode()).hexdigest()

    def _fetch_part(self, src, request, run, logical, *, pinned=False, identity=None, preflight=None,
                    audit_only=False, target_mismatch=None, capture_target=None):
        fingerprint = (identity or request).fingerprint()
        key = self._request_task_key(src, run, logical, fingerprint)
        scope = logical[1:]
        if capture_target is not None:
            # Retain a stable task key and bind its capture to the actual body request and target.
            scope = [*scope, capture_target]
            fingerprint = request.fingerprint()
        with self._write(run) as conn:
            conn.execute(text("""
                INSERT INTO acq_request_tasks (task_key, run_id, kind, scope, fingerprint)
                VALUES (:key, :run, :kind, CAST(:scope AS jsonb), :fingerprint)
                ON CONFLICT (task_key) DO UPDATE SET scope = EXCLUDED.scope, fingerprint = EXCLUDED.fingerprint
                WHERE :rebind
            """), {"key": key, "run": run.id, "kind": logical[0], "scope": json.dumps(scope),
                   "fingerprint": fingerprint, "rebind": capture_target is not None})
        if self.spool is not None and not audit_only and capture_target is None:
            with self._write(run) as conn:
                verify_target = preflight is not None or target_mismatch is not None
                recovered = self._try_spool(conn, run, key, pinned_request=request if verify_target else None)
                if recovered is not None:
                    return recovered
        with self._write(run) as conn:
            self._task_state(conn, key, "queued")
        try:
            result = self._send(src, request, run, key, pinned=pinned, preflight=preflight,
                                audit_only=audit_only, target_mismatch=target_mismatch)
        except SourceBlocked:
            with self._write(run) as conn:
                self._task_state(conn, key, "failed")
            raise
        if isinstance(result, _Failure):
            with self._write(run) as conn:
                self._task_state(conn, key, "failed")
        return result

    def _try_spool(self, conn, run, key, *, pinned_request=None, located_body=False) -> _Receipt | None:
        if self.spool is not None:
            try:
                capture = self.spool.get(key)
                if capture is not None and capture.projection is None:
                    accepted = conn.execute(text("""
                        SELECT a.fingerprint, t.fingerprint AS task_fingerprint, t.scope ->> -1 AS target
                        FROM acq_fetch_attempts a JOIN acq_download_starts d ON d.id = a.download_id
                        JOIN acq_request_tasks t ON t.task_key = d.task_key
                        WHERE a.id = :attempt AND a.run_id = :run AND a.status = 200 AND a.outcome = 'ok'
                          AND d.task_key = :key
                          AND a.fingerprint = d.fingerprint
                          AND a.method = COALESCE(:method, a.method) AND a.url = COALESCE(:route, a.url)
                    """), {"attempt": capture.attempt_id, "run": run.id, "key": key,
                           "method": pinned_request.method if pinned_request is not None else None,
                           "route": _attempt_url(pinned_request.url) if pinned_request is not None else None}).first()
                    if accepted and pinned_request is not None:
                        if not discovery.matches_list_target(pinned_request, capture.response):
                            accepted = None
                    if accepted and located_body:
                        try:
                            target = discovery.request_target(capture.response.url)
                        except TransportError:
                            accepted = None
                        else:
                            if (target[0] != run.origin or _target_digest(target) != accepted.target
                                    or accepted.fingerprint != accepted.task_fingerprint):
                                accepted = None
                    if accepted:
                        return _Receipt(capture.response, capture.attempt_id, capture.received_at, key,
                                        accepted.fingerprint, recovered=True)
                    self.spool.discard(key, capture.attempt_id)
                self.spool.ensure_room()
            except SpoolFull as exc:
                raise _ArchiveDeferred("archive_spool_full") from exc
            except OSError as exc:
                raise _ArchiveDeferred("archive_spool_unavailable") from exc
        return None

    @staticmethod
    def _task_state(conn, key: str, state: str, attempt: int | None = None) -> None:
        conn.execute(text("""
            UPDATE acq_request_tasks
            SET state = :state, last_attempt = COALESCE(:attempt, last_attempt), updated_at = clock_timestamp()
            WHERE task_key = :key
        """), {"key": key, "state": state, "attempt": attempt})

    @contextmanager
    def _settling(self, run, receipts: tuple[_Receipt, ...] | list[_Receipt]):
        with self._write(run) as conn:
            yield conn
            for receipt in receipts:
                self._task_state(conn, receipt.task_key, "settled", receipt.attempt_id)

    def _reject_capture(self, run, receipt: _Receipt) -> None:
        with self._write(run) as conn:
            self._task_state(conn, receipt.task_key, "failed", receipt.attempt_id)
        self._forget_capture(receipt)

    def _persist_capture(self, run, receipt: _Receipt) -> None:
        if self.spool is None or receipt.recovered:
            return
        with self._write(run) as conn:
            if not receipt.streamed:
                capture = Capture(receipt.response, receipt.attempt_id, receipt.received_at)
                try:
                    self.spool.put(receipt.task_key, capture)
                except FileExistsError:
                    if self.spool.get(receipt.task_key) != capture:
                        raise _ArchiveDeferred("archive_spool_conflict") from None
                except SpoolFull as exc:
                    raise _ArchiveDeferred("archive_spool_full") from exc
                except OSError as exc:
                    raise _ArchiveDeferred("archive_spool_unavailable") from exc
            self._task_state(conn, receipt.task_key, "staged", receipt.attempt_id)

    def _forget_capture(self, receipt: _Receipt) -> None:
        if self.spool is not None:
            try:
                self.spool.discard(receipt.task_key, receipt.attempt_id)
            except (OSError, ValueError):
                log.warning("failed to remove committed archive capture attempt=%s", receipt.attempt_id)

    def _windows(self, src: DeclaredSource, window: Window) -> Iterator[Window]:
        if not src.handler.windowed:
            yield window
            return
        for offset in range((window.until - window.since).days + 1):
            day = window.until - timedelta(days=offset)
            yield Window(day, day)

    def _channel(self, src, channel, window, max_pages, refetch, run) -> None:
        handler = src.handler
        report = run.report
        traversal = discovery.ListingTraversal()
        scan = None
        if run.lease.mode in ("backfill", "manual") and run.lease.window.since == run.lease.window.until:
            scan = run.historical_scans[channel] = HistoricalScan()
        list_unverified = False
        rendered_list = src.transport == "browser" and callable(getattr(handler, "browser_list", None))
        channel_origin = None
        for page in range(1, max_pages + 1):
            run.position = (channel, window, page)
            self._require_time(run)
            if rendered_list:
                capture = self._browser_list(src, run, channel_origin)
            else:
                capture = self._http_list(src, run, channel_origin)
            if capture is None:
                return
            resp, listing, browser_binding = capture
            channel_origin = run.origin
            self._require_time(run)
            group_tasks = tuple(self._request_task_key(src, run, self._group_scope(channel, window, group.group_key),
                                                      group.request.fingerprint()) for group in listing.groups)
            kept = discovery.record(lambda: self._settling(run, (resp,)), self.store, source=src.source_id, run=run.id,
                                    attempt=resp.attempt_id, channel=channel, window=window, page=page,
                                    response=resp.response, listing=listing, retained_at=resp.received_at,
                                    days=src.access.list_retention_days, require_storage=self.spool is not None,
                                    group_task_keys=group_tasks, browser_binding=browser_binding)
            if scan is not None and (kept or self.spool is None):
                scan.pages.append((resp.attempt_id, len(listing.refs or listing.groups)))
            if not kept:
                if self.spool is not None:
                    raise _ArchiveDeferred("list_archive_pending")
                self._gap(run, channel, window, page, "list_archive_write")
                self._failed(src, resp.response.url, _Failure("list_archive_write", True), run)
            report.pages += 1
            if resp.recovered:
                self._gap(run, channel, window, page, "recovered_list_requires_rescan")
            issues = traversal.observe(listing, page=page)
            if scan is not None and any(ref.published is None for ref in listing.refs):
                issues.append(initial_backfill.LIST_DATE_UNAVAILABLE)
            for issue in issues:
                self._gap(run, channel, window, page, issue)
            past_window = traversal.past_window(window)
            coverage_gap = listing.coverage_gap is not None and not (
                past_window and listing.coverage_gap in initial_backfill.ARCHIVE_BOUNDARIES)
            empty_with_next = not listing.refs and not listing.groups and listing.has_next
            list_unverified |= bool(not kept or resp.recovered or issues or coverage_gap or empty_with_next)
            # The list can reach its endpoint before a required original is archived.
            if scan is not None and not list_unverified and (not listing.has_next or past_window):
                scan.end = "ordered_cutoff" if listing.has_next else "terminal"
            if group_tasks:
                self._drain_groups(src, run)
                run.position = (channel, window, page)
            if "repeated_page" in issues or "next_page_empty" in issues:
                self._forget_capture(resp)
                return
            for ref in listing.refs:
                if ref.published is not None and ref.published > window.until:
                    continue
                if ref.published is not None and ref.published < window.since:
                    continue
                report.listed += 1
                self._require_time(run)
                failed = report.failed
                pending = report.pending_identity
                if src.transport == "browser":
                    from . import browser_jobs

                    if self._needs_detail(src, ref, refetch, run):
                        browser_jobs.collect(self, src, channel, ref, resp.attempt_id, run)
                else:
                    self._detail(src, channel, ref, refetch, run)
                if report.failed > failed:
                    self._gap(run, channel, window, page, report.failures[-1]["error_code"])
                if report.pending_identity > pending:
                    self._gap(run, channel, window, page, "identity_pending")
            self._forget_capture(resp)
            if coverage_gap:
                self._gap(run, channel, window, page, listing.coverage_gap)
            if empty_with_next:
                self._gap(run, channel, window, page, "empty_list_with_next")
                return
            if not listing.has_next or past_window:
                return
        self._gap(run, channel, window, max_pages + 1, "page_limit")

    def _http_list(self, src, run, channel_origin):
        channel, window, page = run.position
        handler = src.handler
        try:
            selected = discovery.listing_request(handler, channel, window, page, self.clock.now())
            if selected.preflight is not None and channel_origin not in (None, selected.origin):
                raise discovery.ListPlanError("target_outside_channel", selected.request.url)
        except discovery.ListPlanError as exc:
            self._list_failure(src, run, exc.url, _Failure(exc.code, False))
            return None
        run.origin = selected.origin
        request = selected.request
        resp = self._fetch_part(src, request, run, ["list", channel, window.since.isoformat(), window.until.isoformat(), page],
                                identity=selected.identity, pinned=selected.preflight is not None,
                                preflight=selected.preflight)
        if isinstance(resp, _Failure):
            self._list_failure(src, run, run.last_url if selected.preflight is not None else request.url, resp)
            return None
        try:
            listing = handler.parse_list(channel, resp.response).for_request(page, window=window if handler.windowed else None)
            if listing.groups and not callable(getattr(handler, "group_request", None)):
                raise ValueError("grouped discovery requires a recoverable group request")
        except SourceBlocked:
            self._reject_capture(run, resp)
            raise
        except ValueError:
            self._reject_capture(run, resp)
            self._list_failure(src, run, request.url, _Failure("invalid_list", False))
            return None
        self._persist_capture(run, resp)
        return resp, listing, None

    def _browser_list(self, src, run, channel_origin):
        from . import browser_discovery

        try:
            plan, origin = browser_discovery.plan_for(src.handler, *run.position, channel_origin)
        except discovery.ListPlanError as exc:
            self._list_failure(src, run, exc.url, _Failure(exc.code, False))
            return None
        run.origin = origin
        result = browser_discovery.collect(self, src, plan, run)
        if isinstance(result, _Failure):
            self._list_failure(src, run, plan.body_request.url, result)
            return None
        return result

    def _list_failure(self, src, run, url, failure):
        self._gap(run, *run.position, failure.code)
        self._failed(src, url, failure, run)

    @staticmethod
    def _group_scope(channel: str, window: Window, key: str) -> list[str]:
        return ["group", channel, window.since.isoformat(), window.until.isoformat(), key]

    def _drain_groups(self, src: DeclaredSource, run: _Run) -> None:
        with self.db.tx() as conn:
            tasks = conn.execute(text("""
                SELECT task_key, scope, fingerprint, state, last_attempt
                FROM acq_request_tasks
                WHERE run_id = :run AND kind = 'group' AND state IN ('queued', 'staged')
                ORDER BY (scope->>4)::bigint, task_key
            """), {"run": run.id}).all()
        for task in tasks:
            self._require_time(run)
            channel, since, until, key, list_attempt, page, *checkpoint = task.scope
            window = Window(date.fromisoformat(since), date.fromisoformat(until))
            run.position = (channel, window, page)
            try:
                request = src.handler.group_request(channel, key)
                group = NoticeGroup(key, request)
                logical = self._group_scope(channel, window, key)
                listing = discovery.listing_request(src.handler, channel, window, page, self.clock.now())
                run.origin = listing.origin
                if (request.fingerprint() != task.fingerprint or request_origin(request.url) != run.origin
                        or self._request_task_key(src, run, logical, task.fingerprint) != task.task_key):
                    raise ValueError("group task no longer matches its discovery request")
            except (ValueError, TypeError, TransportError):
                with self._write(run) as conn:
                    self._task_state(conn, task.task_key, "failed")
                self._gap(run, channel, window, page, "invalid_group_request")
                self._failed(src, "", _Failure("invalid_group_request", False), run)
                continue
            receipt = self._group_object(run, task, request, checkpoint[0]) if checkpoint else None
            from_archive = receipt is not None
            if receipt is None:
                receipt = self._fetch_part(src, request, run, logical, pinned=True,
                                           target_mismatch="group_response_target_mismatch")
            if isinstance(receipt, _Failure):
                self._gap(run, channel, window, page, receipt.code)
                self._failed(src, request.url, receipt, run)
                continue
            self._archive_group(src, run, group, receipt, list_attempt, from_archive=from_archive)

    def _group_object(self, run: _Run, task, request: FetchRequest, checkpoint: dict) -> _Receipt | None:
        if task.state != "staged" or task.last_attempt != checkpoint["attempt_id"]:
            return None
        with self.db.tx() as conn:
            self._check_rights(conn, run)
        digest = checkpoint["content_hash"]
        size = checkpoint["size_bytes"]
        try:
            raw = self.store.get(f"raw/{digest.removeprefix('sha256:')}", max_bytes=size)
        except FileNotFoundError:
            return None
        except ValueError as exc:
            raise _ArchiveDeferred("group_object_conflict") from exc
        except OSError as exc:
            raise _ArchiveDeferred("group_object_pending") from exc
        if len(raw) != size or f"sha256:{hashlib.sha256(raw).hexdigest()}" != digest:
            raise _ArchiveDeferred("group_object_conflict")
        received_at = datetime.fromisoformat(checkpoint["received_at"])
        return _Receipt(FetchResponse(200, checkpoint["media_type"], raw, request.url), task.last_attempt,
                        received_at, task.task_key, task.fingerprint, recovered=True)

    def _archive_group(self, src: DeclaredSource, run: _Run, group: NoticeGroup, receipt: _Receipt,
                       list_attempt: int, *, from_archive: bool) -> None:
        from . import grouped_observations

        channel, window, page = run.position
        try:
            capture = grouped_observations.validate_group(src.handler, channel, group, receipt.response)
        except SourceBlocked:
            self._reject_capture(run, receipt)
            raise
        except (ValueError, TypeError, KeyError, IndexError):
            self._reject_capture(run, receipt)
            self._gap(run, channel, window, page, "invalid_group_content")
            self._failed(src, group.request.url, _Failure("invalid_group_content", False), run)
            return
        self._persist_capture(run, receipt)
        with self._write(run) as conn:
            checkpoint = {"attempt_id": receipt.attempt_id, "content_hash": capture.content_hash,
                          "media_type": capture.media_type, "size_bytes": capture.size_bytes,
                          "received_at": _instant(receipt.received_at)}
            conn.execute(text("""
                UPDATE acq_request_tasks SET scope = jsonb_set(scope, '{6}', CAST(:checkpoint AS jsonb))
                WHERE task_key = :task
            """), {"task": receipt.task_key, "checkpoint": json.dumps(checkpoint)})
            self._task_state(conn, receipt.task_key, "staged", receipt.attempt_id)
        if not from_archive:
            self._stage(src, group.group_key, group.request.url, receipt, run, durable=True)
        try:
            with self._write(run) as conn:
                result = grouped_observations.submit_group(conn, lease=run.lease, window=window,
                                                           list_attempt_id=list_attempt, task_key=receipt.task_key,
                                                           attempt_id=receipt.attempt_id,
                                                           received_at=receipt.received_at, capture=capture)
        except grouped_observations.GroupConflict:
            self._reject_capture(run, receipt)
            self._gap(run, channel, window, page, "group_observation_conflict")
            self._failed(src, group.request.url, _Failure("group_observation_conflict", False), run)
            return
        run.report.listed += len(capture.members) - result.outside_window
        run.report.archived += result.archived
        run.report.unchanged += result.unchanged
        run.report.skipped += result.stale
        if result.stale and channel in run.historical_scans:
            self._gap(run, channel, window, page, "stale_group_member_requires_recheck")
        if receipt.recovered:
            self._gap(run, channel, window, page, "recovered_group_requires_rescan")
        self._forget_capture(receipt)

    def _needs_detail(self, src: DeclaredSource, ref: DetailRef, refetch: bool, run: _Run) -> bool:
        report = run.report
        key = {"s": src.source_id, "k": ref.source_key}
        if ref.source_key in run.checked:
            report.skipped += 1
            return False
        if ref.source_key is not None:
            run.attempted.add(ref.source_key)
            if not refetch:
                with self.db.tx() as conn:
                    already_checked = conn.execute(text("""
                        SELECT 1 FROM acq_detail_checks c
                        JOIN acq_source_observations o USING (source_id, notice_key, version)
                        WHERE c.source_id = :s AND c.notice_key = :k
                        AND (CAST(:published AS date) IS NULL OR o.published_on = :published)
                        AND NOT EXISTS (
                            SELECT 1 FROM acq_observation_attachments a
                            WHERE (a.source_id, a.notice_key, a.version) = (c.source_id, c.notice_key, c.version)
                              AND a.status = 'pending'
                        )
                    """), {**key, "published": ref.published}).first() is not None
                    if already_checked and run.position is not None and run.historical_scans:
                        day = run.position[1].since.isoformat()
                        statuses = original_statuses(conn, src.source_id, {ref.source_key})
                        already_checked = statuses.get((ref.source_key, day)) == "archived"
                    if already_checked:
                        report.skipped += 1
                        return False
        return True

    def _detail(self, src: DeclaredSource, channel: str, ref: DetailRef, refetch: bool, run: _Run) -> None:
        if not self._needs_detail(src, ref, refetch, run):
            return
        report = run.report
        declarations = src.handler.detail_fetches(channel, ref)
        bodies = [part for part in declarations if part.kind == "body"]
        if len(bodies) != 1:
            self._failed(src, ref.source_url, _Failure("unsupported_detail_parts", False), run)
            return
        fetch = bodies[0]
        logical = ["body", ref.source_key, fetch.artifact_key]
        if fetch.locator is None:
            assert fetch.request is not None
            receipt = self._fetch_part(src, fetch.request, run, logical, pinned=True)
        else:
            receipt = self._located_body(src, channel, ref, fetch.locator, run, logical)
        if isinstance(receipt, _Failure):
            self._failed(src, ref.source_url, receipt, run)
            return
        try:
            ref = checked_detail_ref(src.handler, ref, receipt.response)
        except SourceBlocked:
            self._reject_capture(run, receipt)
            raise
        except ValueError:
            self._reject_capture(run, receipt)
            self._failed(src, ref.source_url, _Failure("invalid_content", False), run)
            return
        if run.position is not None and ref.published is not None:
            window = run.position[1]
            if not window.since <= ref.published <= window.until:
                with self._settling(run, (receipt,)):
                    pass
                self._forget_capture(receipt)
                report.skipped += 1
                return
        self._persist_capture(run, receipt)
        part_key = fetch.artifact_key if ref.source_key is not None else f"pending:{receipt.request_fingerprint}"
        artifact = self._stage(src, part_key, ref.source_url, receipt, run)
        if isinstance(artifact, _Failure):
            self._reject_capture(run, receipt)
            self._failed(src, ref.source_url, artifact, run)
            return
        if ref.source_key is None:
            with self._settling(run, (receipt,)) as conn:
                artifact_id = self._artifact(conn, artifact)
                conn.execute(text("""
                    INSERT INTO acq_pending_identity (attempt_id, artifact_id, source_url, observed_at)
                    VALUES (:attempt, :a, :u, :t) ON CONFLICT (attempt_id) DO NOTHING
                """), {"attempt": receipt.attempt_id, "a": artifact_id, "u": ref.source_url, "t": artifact["t"]})
            report.pending_identity += 1
            self._forget_capture(receipt)
            return
        # Only the optional hook can admit attachments. It sees the validated body.
        discover = getattr(src.handler, "attachment_fetches", None)
        parts = ()
        if discover is not None:
            try:
                parts = tuple(discover(channel, ref, receipt.response))
                keys = {part.artifact_key for part in parts}
                if (len(parts) > MAX_ATTACHMENTS or len(parts) != len(keys)
                        or any(part.kind != "attachment" for part in parts)):
                    raise ValueError("invalid attachment manifest")
            except SourceBlocked as exc:
                scan = run.historical_scans.get(channel)
                if scan is not None:
                    scan.manifest_unverified = True
                self._archive_body(src, channel, ref, artifact, receipt, (), run)
                self._forget_capture(receipt)
                raise exc
            except ValueError:
                self._attachment_issue(channel, ref, None, "invalid_attachment_manifest", run)
                parts = ()
        if len(declarations) != 1:
            self._attachment_issue(channel, ref, None, "unverified_attachment_declaration", run)
        archived = self._archive_body(src, channel, ref, artifact, receipt, parts, run)
        if isinstance(archived, _Failure):
            self._failed(src, ref.source_url, archived, run)
        if archived is None or isinstance(archived, _Failure):
            self._forget_capture(receipt)
            return
        version, attached, parts = archived
        if parts:
            self._collect_attachments(src, channel, ref, artifact, receipt, version, attached, parts, refetch, run)
        self._forget_capture(receipt)

    def _located_body(self, src, channel, ref, locator, run, logical) -> _Receipt | _Failure:
        body_scope = [*logical, "located"]
        key = self._request_task_key(src, run, body_scope, locator.fingerprint())
        # An already captured original remains recoverable if the public locator is unavailable.
        if self.spool is not None:
            with self._write(run) as conn:
                recovered = self._try_spool(conn, run, key, located_body=True)
                if recovered is not None:
                    return recovered
        mismatch = "detail_response_target_mismatch"
        receipt = self._fetch_part(src, locator, run, [*logical, "locator"], pinned=True, audit_only=True,
                                   target_mismatch=mismatch)
        if isinstance(receipt, _Failure):
            return receipt
        try:
            request = resolve_detail_request(src.handler, channel, ref, receipt.response)
        except (SourceBlocked, ValueError, TypeError, KeyError, IndexError) as exc:
            with self._write(run) as conn:
                self._task_state(conn, receipt.task_key, "failed", receipt.attempt_id)
            if isinstance(exc, SourceBlocked):
                raise
            return _Failure("invalid_detail_locator", False)
        with self._settling(run, (receipt,)):
            pass
        try:
            target = discovery.request_target(request.url)
        except TransportError:
            return _Failure("invalid_target", False)
        if target[0] != run.origin:
            return _Failure("target_outside_channel", False)
        return self._fetch_part(src, request, run, body_scope, pinned=True, identity=locator,
                                target_mismatch=mismatch, capture_target=_target_digest(target))

    def _stage(self, src, part_key, source_url, receipt, run, *, durable: bool = False) -> dict | _Failure:
        self._require_time(run)
        with self.db.tx() as conn:
            self._check_rights(conn, run)
        self._require_time(run)
        response = receipt.response
        digest = hashlib.sha256(response.body).hexdigest()
        artifact_ref = f"raw/{digest}"
        try:
            self.store.put(artifact_ref, response.body)
            if self.spool is not None or durable:
                try:
                    verified = self.store.get(artifact_ref, max_bytes=len(response.body))
                except ValueError as exc:
                    raise ObjectConflict(artifact_ref) from exc
                if verified != response.body:
                    raise ObjectConflict(artifact_ref)
        except (OSError, ObjectConflict) as exc:
            if self.spool is not None or durable:
                raise _ArchiveDeferred("archive_object_conflict" if isinstance(exc, ObjectConflict)
                                       else "archive_object_pending") from exc
            return _Failure("archive_write", isinstance(exc, OSError))
        return {"s": src.source_id, "part": part_key, "h": f"sha256:{digest}", "a": artifact_ref,
                "m": response.media_type, "n": len(response.body), "u": source_url,
                "f": receipt.request_fingerprint, "t": receipt.received_at}

    @staticmethod
    def _latest(conn, key):
        return conn.execute(text("""
            SELECT o.version, o.artifact_id, a.content_hash, c.attempt_id, c.legacy_artifact_id, attempt.run_id,
                   o.selector IS NOT NULL AS grouped, o.browser_attempt_id, o.published_on
            FROM acq_detail_checks c JOIN acq_source_observations o USING (source_id, notice_key, version)
            JOIN acq_raw_artifacts a ON a.id = o.artifact_id
            JOIN acq_fetch_attempts attempt ON attempt.id = c.attempt_id
            WHERE c.source_id = :s AND c.notice_key = :k
        """), key).first()

    @staticmethod
    def _parts(conn, key, version: int) -> dict[str, _Attachment]:
        return {row["part_key"]: _Attachment(**row) for row in conn.execute(text("""
            SELECT p.part_key, p.required, p.name, p.request_fingerprint, p.artifact_id, p.attempt_id,
                   a.artifact_ref, a.content_hash, a.media_type, a.size_bytes
            FROM acq_observation_attachments p
            LEFT JOIN acq_raw_artifacts a ON (a.source_id, a.id) = (p.source_id, p.artifact_id)
            WHERE p.source_id = :s AND p.notice_key = :k AND p.version = :v
            ORDER BY p.part_key
        """), {**key, "v": version}).mappings()}

    def _archive_body(self, src, channel, ref, artifact, receipt, parts, run):
        key = {"s": src.source_id, "k": ref.source_key}
        with self._write(run) as conn:
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": f"{src.source_id}|{ref.source_key}"})
            latest = self._latest(conn, key)
            if latest is not None and latest.browser_attempt_id is not None:
                self._task_state(conn, receipt.task_key, "failed", receipt.attempt_id)
                return _Failure("browser_observation_conflict", False)
            if latest is not None and latest.grouped:
                self._task_state(conn, receipt.task_key, "failed", receipt.attempt_id)
                return _Failure("group_observation_conflict", False)
            self._task_state(conn, receipt.task_key, "settled", receipt.attempt_id)
            if latest and (latest.run_id, latest.attempt_id) >= (run.id, receipt.attempt_id):
                run.report.skipped += 1
                if (receipt.recovered
                        and (latest.run_id, latest.attempt_id) == (run.id, receipt.attempt_id)
                        and latest.content_hash == artifact["h"]):
                    run.checked.add(ref.source_key)
                    saved = self._parts(conn, key, latest.version)
                    return latest.version, saved, tuple(part for part in parts if part.artifact_key in saved)
                return None
            version = latest.version if latest else 0
            previous = self._parts(conn, key, version) if latest else {}
            same_body = latest is not None and latest.content_hash == artifact["h"]
            attached = previous.copy() if same_body else {}
            if len(attached.keys() | {part.artifact_key for part in parts}) > MAX_ATTACHMENTS:
                self._attachment_issue(channel, ref, None, "attachment_manifest_over_limit", run)
                parts = ()
            for part in parts:
                fingerprint = part.request.fingerprint() if part.request is not None else None
                saved = attached.get(part.artifact_key)
                if saved is not None and saved.request_fingerprint == fingerprint:
                    attached[part.artifact_key] = replace(saved, required=part.required, name=part.name)
                else:
                    attached[part.artifact_key] = _Attachment(part.artifact_key, part.required, part.name, fingerprint)
            legacy = latest.legacy_artifact_id if latest else conn.execute(text("""
                SELECT max(a.id) FROM acq_raw_artifacts a
                WHERE a.source_id = :s AND a.source_key = :part AND NOT EXISTS (
                    SELECT 1 FROM acq_source_observations o WHERE o.artifact_id = a.id
                )
                HAVING count(DISTINCT a.source_url) = 1
            """), artifact).scalar_one_or_none()
            published = latest.published_on if same_body and ref.published is None else ref.published
            changed = not same_body or attached != previous or published != latest.published_on
            check = {**key, "v": version + int(changed), "attempt": receipt.attempt_id,
                     "t": artifact["t"], "legacy": legacy}
            if changed:
                artifact_id = self._artifact(conn, artifact)
                self._append_observation(conn, src, channel, ref, artifact, artifact_id, attached, check, published)
            self._advance_check(conn, check)
        run.checked.add(ref.source_key)
        if changed:
            run.report.archived += 1
        else:
            run.report.unchanged += 1
        return check["v"], attached, parts

    def _collect_attachments(self, src, channel, ref, body, receipt, version, attached, parts, refetch, run):
        staged = {}
        captures = []
        try:
            for part in parts:
                saved = attached[part.artifact_key]
                if not refetch and saved.artifact_id is not None:
                    continue
                run.attachment = (ref.source_key, version, part.artifact_key)
                try:
                    result = self._download_attachment(src, ref, part, run, receipt.attempt_id)
                except SourceBlocked as exc:
                    self._attachment_issue(channel, ref, part, exc.code, run)
                    if saved.artifact_id is not None:
                        staged[part.artifact_key] = None
                    raise
                except _DeadlineReached:
                    self._attachment_issue(channel, ref, part, "attachment_deadline", run)
                    raise
                if isinstance(result, _Failure):
                    self._attachment_issue(channel, ref, part, result.code, run)
                    if saved.artifact_id is not None:
                        staged[part.artifact_key] = None
                else:
                    raw, attachment = result
                    captures.append(attachment)
                    if saved.content_hash != raw["h"]:
                        staged[part.artifact_key] = (raw, attachment.attempt_id)
        except SourceBlocked:
            self._archive_attachments(src, channel, ref, body, receipt, version, attached, staged, captures, run)
            for capture in captures:
                self._forget_capture(capture)
            raise
        finally:
            run.attachment = None
        self._archive_attachments(src, channel, ref, body, receipt, version, attached, staged, captures, run)
        for capture in captures:
            self._forget_capture(capture)

    def _download_attachment(self, src, ref, part, run, body_attempt_id) -> tuple[dict, _Receipt] | _Failure:
        if part.request is None:
            return _Failure("attachment_target_missing", False)
        result = self._fetch_part(src, part.request, run,
                                  ["attachment", ref.source_key, body_attempt_id, part.artifact_key], pinned=True)
        if isinstance(result, _Failure):
            return result
        response = result.response
        if (not 1 <= len(response.body) <= MAX_BODY or not 1 <= len(response.media_type) <= 128
                or response.media_type in ("text/html", "application/xhtml+xml", "application/json")):
            self._reject_capture(run, result)
            return _Failure("invalid_attachment_content", False)
        self._persist_capture(run, result)
        # Catalog identity remains distinct from both the body and another notice with the same part key.
        stable = hashlib.sha256(json.dumps([ref.source_key, part.artifact_key]).encode()).hexdigest()
        raw = self._stage(src, f"attachment:{stable}", _attempt_url(part.request.url), result, run)
        if isinstance(raw, _Failure):
            self._reject_capture(run, result)
            return raw
        return raw, result

    def _archive_attachments(self, src, channel, ref, body, receipt, version, attached, staged, captures, run):
        if not staged and not captures:
            return
        with self._settling(run, captures) as conn:
            if not staged:
                return
            key = {"s": src.source_id, "k": ref.source_key}
            latest = self._latest(conn, key)
            if (latest is None or (latest.run_id, latest.attempt_id, latest.version) != (run.id, receipt.attempt_id, version)
                    or latest.content_hash != body["h"]):
                return
            for part_key, result in staged.items():
                if result is None:
                    part = attached[part_key]
                    attached[part_key] = _Attachment(part_key, part.required, part.name, part.request_fingerprint)
                    continue
                raw, attempt_id = result
                artifact_id = self._artifact(conn, raw)
                media_type = conn.execute(text("SELECT media_type FROM acq_raw_artifacts WHERE id = :id"),
                                          {"id": artifact_id}).scalar_one()
                attached[part_key] = replace(attached[part_key], artifact_id=artifact_id,
                                             attempt_id=attempt_id, artifact_ref=raw["a"], content_hash=raw["h"],
                                             media_type=media_type, size_bytes=raw["n"])
            check = {**key, "v": version + 1, "attempt": receipt.attempt_id, "t": self.clock.now(),
                     "legacy": latest.legacy_artifact_id}
            self._append_observation(conn, src, channel, ref, body, latest.artifact_id, attached, check, latest.published_on)
            self._advance_check(conn, check)
        run.report.archived += 1

    def _append_observation(self, conn, src, channel, ref, body, artifact_id, parts, check, published):
        conn.execute(text("""
            INSERT INTO acq_source_observations
                (source_id, notice_key, version, artifact_id, attempt_id, source_url, published_on, observed_at, channel)
            VALUES (:s, :k, :v, :a, :attempt, :u, :p, :t, :channel)
        """), {**check, "a": artifact_id, "u": ref.source_url, "p": published, "channel": channel})
        event = {"source_id": src.source_id, "notice_key": ref.source_key, "observation_version": check["v"],
                 "artifact_ref": body["a"], "rights_tags": rights_tags(src.source_id),
                 "content_hash": body["h"], "fetched_at": _instant(check["t"]), "source_url": ref.source_url}
        if parts:
            conn.execute(text("""
                INSERT INTO acq_observation_attachments
                    (source_id, notice_key, version, part_key, required, name, request_fingerprint, status, artifact_id, attempt_id)
                VALUES (:s, :k, :v, :part, :required, :name, :fingerprint, :status, :artifact, :part_attempt)
            """), [{**check, "part": part.part_key, "required": part.required, "name": part.name,
                   "fingerprint": part.request_fingerprint, "status": "available" if part.artifact_id else "pending",
                   "artifact": part.artifact_id, "part_attempt": part.attempt_id} for part in parts.values()])
            event["attachments"] = [parts[part_key].event() for part_key in sorted(parts)]
        if check["legacy"] is not None:
            legacy = conn.execute(text("""
                SELECT source_url, content_hash FROM acq_raw_artifacts WHERE id = :id
            """), {"id": check["legacy"]}).mappings().one()
            event["legacy_artifact"] = dict(legacy)
        writer.append(conn, outbox.Event(type=NOTICE_ARCHIVED, subject=body["a"],
                                         partition_key=f"{src.source_id}|{ref.source_key}", data=event))

    @staticmethod
    def _advance_check(conn, check):
        conn.execute(text("""
            INSERT INTO acq_detail_checks (source_id, notice_key, version, attempt_id, checked_at, legacy_artifact_id)
            VALUES (:s, :k, :v, :attempt, :t, :legacy)
            ON CONFLICT (source_id, notice_key) DO UPDATE SET version = EXCLUDED.version,
                attempt_id = EXCLUDED.attempt_id, checked_at = EXCLUDED.checked_at
        """), check)

    def _recover_attachments(self, src, run):
        if not hasattr(src.handler, "attachment_fetches"):
            return
        with self.db.tx() as conn:
            pending = conn.execute(text("""
                SELECT o.notice_key, o.source_url, o.published_on, o.channel
                FROM acq_detail_checks c JOIN acq_source_observations o USING (source_id, notice_key, version)
                WHERE c.source_id = :source AND o.channel = ANY(:channels)
                  AND NOT (o.notice_key = ANY(:attempted))
                  AND EXISTS (
                    SELECT 1 FROM acq_observation_attachments p
                    WHERE (p.source_id, p.notice_key, p.version) = (c.source_id, c.notice_key, c.version)
                      AND p.status = 'pending'
                )
                ORDER BY c.checked_at, o.notice_key LIMIT :limit
            """), {"source": src.source_id, "channels": list(src.handler.channels),
                   "attempted": list(run.attempted), "limit": MAX_RECOVERY_NOTICES}).all()
        run.position = None
        for row in pending:
            self._require_time(run)
            request = src.handler.list_request(row.channel, run.lease.window, 1)
            run.origin = request_origin(request.url)
            self._detail(src, row.channel, DetailRef(row.notice_key, row.source_url, row.published_on), False, run)

    @staticmethod
    def _attachment_issue(channel, ref, part, reason, run):
        if part is None and (scan := run.historical_scans.get(channel)) is not None:
            scan.manifest_unverified = True
        issue = {"channel": channel, "notice_key": ref.source_key, "reason": reason}
        if part is not None:
            issue["part_key"] = part.artifact_key
        run.report.failures.append(issue.copy())
        if part is None or part.required:
            if run.position is not None:
                _, window, page = run.position
                issue.update(since=window.since.isoformat(), until=window.until.isoformat(), page=page)
            run.report.gaps.append(issue)

    @staticmethod
    def _check_rights(conn, run):
        allowed = conn.execute(text("SELECT public.acq_collection_allowed(:source)"),
                               {"source": run.report.source_id}).scalar_one()
        if not allowed:
            raise _RightsRevoked(run.report.source_id)

    @staticmethod
    def _artifact(conn: Connection, artifact: dict) -> int:
        inserted = conn.execute(text("""
            INSERT INTO acq_raw_artifacts (source_id, source_key, content_hash, artifact_ref, media_type, size_bytes,
                                          source_url, request_fingerprint, fetched_at)
            VALUES (:s, :part, :h, :a, :m, :n, :u, :f, :t)
            ON CONFLICT (source_id, source_key, content_hash) DO NOTHING RETURNING id
        """), artifact).scalar_one_or_none()
        if inserted is not None:
            return inserted
        return conn.execute(text("""
            SELECT id FROM acq_raw_artifacts WHERE source_id = :s AND source_key = :part AND content_hash = :h
        """), artifact).scalar_one()

    # ---- requests -------------------------------------------------------------------------------------------
    def _send(self, src: DeclaredSource, request: FetchRequest, run: _Run, task_key: str, *,
              pinned: bool = False, preflight: FetchRequest | None = None, audit_only: bool = False,
              target_mismatch: str | None = None) -> _Receipt | _Failure:
        """Download one logical part through the same request path as list pages and body."""
        access = src.access
        try:
            origin = request_origin(request.url)
        except TransportError:
            return _Failure("invalid_target", False)
        default_headers = access.headers
        if origin != run.origin:
            if not pinned or run.attachment is None or origin not in map(request_origin, access.attachment_origins):
                return _Failure("target_outside_channel", False)
            default_headers = ()
        request = FetchRequest(request.method, request.url, request.form, request.json_body,
                               default_headers + request.headers)
        failure = _Failure("exhausted", True)
        if preflight is not None:
            target_mismatch = "list_response_target_mismatch"
        for attempt in range(1, access.max_attempts + 1):
            if preflight is not None:
                rejected = self._preflight(src, preflight, run, task_key)
                if rejected is not None:
                    return rejected
            result = self._follow(src, request, run, task_key, attempt, pinned,
                                  audit_only=audit_only, target_mismatch=target_mismatch)
            if isinstance(result, _Receipt) or not result.retryable:
                return result
            if target_mismatch is not None and result.code == "throttled":
                return result
            failure = result
        return failure

    def _preflight(self, src, request, run, task_key) -> _Failure | None:
        receipt = self._send(src, request, run, task_key, pinned=True, audit_only=True,
                             target_mismatch="list_response_target_mismatch")
        if isinstance(receipt, _Failure):
            return receipt
        try:
            src.handler.check_list_preflight(receipt.response)
        except (SourceBlocked, ValueError, TypeError, KeyError, IndexError) as exc:
            with self._write(run) as conn:
                self._task_state(conn, task_key, "failed", receipt.attempt_id)
            if isinstance(exc, SourceBlocked):
                raise
            return _Failure("invalid_list_preflight", False)
        return None

    def _follow(self, src, request, run, task_key, attempt, pinned, *, audit_only, target_mismatch) -> _Failure | _Receipt:
        for hop in range(MAX_REDIRECTS + 1):
            result = self._hop(src, request, run, task_key, attempt, audit_only=audit_only,
                               target_mismatch=target_mismatch)
            if not isinstance(result, _Receipt) or result.response.status not in REDIRECTS:
                return result
            if pinned:
                return _Failure("part_redirect_disallowed", False)
            if hop == MAX_REDIRECTS:
                return _Failure("redirect_limit", False)
            try:
                request = redirect_request(request, result.response)
            except TransportError:
                return _Failure("redirect_disallowed", False)
        raise AssertionError("unreachable redirect state")

    def _hop(self, src, request, run, task_key, attempt, *, audit_only, target_mismatch) -> _Receipt | _Failure:
        with self._admit_hop(src, request, run, task_key, capture=not audit_only) as (slot, download_id, incoming):
            return self._transfer(slot, src, request, run, task_key, attempt, download_id, incoming,
                                  target_mismatch=target_mismatch)

    @contextmanager
    def _admit_hop(self, src, request, run, task_key, *, capture: bool, deadline: float | None = None):
        """Hold the shared site/source permit across one already validated network request."""
        access = src.access
        run.last_url = request.url
        site = site_identity(request.url)
        until = run.deadline if deadline is None else min(run.deadline, deadline)
        renew_at = self.clock.monotonic() + LEASE_WAIT_STEP_SECONDS
        while True:
            if self.clock.monotonic() >= until:
                raise _DeadlineReached
            if self.clock.monotonic() >= renew_at:
                with self._write(run):
                    pass
                renew_at = self.clock.monotonic() + LEASE_WAIT_STEP_SECONDS
            with self.slots.try_acquire(source=src.source_id, site=site, mode=run.lease.mode) as slot, ExitStack() as cleanup:
                if isinstance(slot, float):
                    delay = slot
                else:
                    with slot.connection.begin():
                        now, delay = slot.availability(access.min_interval_seconds)
                        if not delay:
                            if self.clock.monotonic() >= until:
                                raise _DeadlineReached
                            run.lease.check(slot.connection)
                            self._check_rights(slot.connection, run)
                            incoming = cleanup.enter_context(self._incoming_capture()) if capture else None
                            download_id = slot.dispatch(request, run_id=run.id, generation=run.lease.generation,
                                                        at=now, interval=access.min_interval_seconds,
                                                        task_key=task_key)
                    if not delay:
                        yield slot, download_id, incoming
                        return
            # Do not hold a download slot while waiting for pacing or another transfer.
            if delay >= until - self.clock.monotonic():
                raise _DeadlineReached
            self.clock.sleep(min(delay, LEASE_WAIT_STEP_SECONDS))

    @contextmanager
    def _incoming_capture(self) -> Iterator[IncomingCapture | None]:
        if self.spool is None or self._stream is None:
            yield None
            return
        try:
            with self.spool.receive() as incoming:
                yield incoming
        except SpoolFull as exc:
            raise _ArchiveDeferred("archive_spool_full") from exc
        except FileExistsError as exc:
            raise _ArchiveDeferred("archive_spool_conflict") from exc
        except OSError as exc:
            raise _ArchiveDeferred("archive_spool_unavailable") from exc

    def _transfer(self, slot, src, request, run, task_key, attempt, download_id, incoming, *,
                  target_mismatch) -> _Receipt | _Failure:
        self._require_time(run)
        access = src.access
        started = self.clock.monotonic()
        timeout = min(access.timeout_seconds, self._remaining(run))
        delay = access.min_interval_seconds * 2 ** attempt
        try:
            if incoming is None:
                response = self.transport.send(request, timeout)
            else:
                response = self._stream(request, timeout, incoming.write)
                if response.status == 200:
                    response = replace(response, body=incoming.body())
        except SpoolWriteError as exc:
            self._attempt(slot.connection, run, request, download_id, None, "invalid", started, "archive_spool_unavailable")
            raise _ArchiveDeferred("archive_spool_unavailable") from exc
        except TransportError as exc:
            if exc.retryable:
                slot.cooldown(delay)
            self._attempt(slot.connection, run, request, download_id, None, "network_error", started, str(exc))
            self._require_time(run)
            return _Failure("network_error", exc.retryable)
        status = response.status
        failure = None
        outcome = "ok"
        if status in (401, 403):
            outcome = "blocked"
        elif status == 429 or (target_mismatch is not None and response.retry_after is not None):
            outcome, failure = "throttled", _Failure("throttled", True)
            delay = max(access.throttle_backoff_seconds * 2 ** (attempt - 1),
                        _retry_after(response.retry_after, self.clock.now()))
            slot.cooldown(delay, shared_site=True)
        elif target_mismatch is not None and status == 200 and not discovery.matches_list_target(request, response):
            outcome, failure = "invalid", _Failure(target_mismatch, False)
        elif status != 200 and status not in REDIRECTS:
            outcome, failure = "http_error", _Failure(f"http_{status}", status >= 500)
            if failure.retryable:
                slot.cooldown(delay)
        attempt_id = self._attempt(slot.connection, run, request, download_id, status, outcome, started, None)
        self._require_time(run)
        if outcome == "blocked":
            raise SourceBlocked(f"http_{status}")
        if failure is not None:
            return failure
        received = self.clock.now()
        streamed = incoming is not None and status == 200
        if streamed:
            try:
                with slot.connection.begin():
                    run.lease.check(slot.connection)
                    self._check_rights(slot.connection, run)
                    incoming.publish(task_key, Capture(response, attempt_id, received))
            except InvalidCaptureMetadata:
                return _Failure("invalid_capture_metadata", False)
        return _Receipt(response, attempt_id, received, task_key, request.fingerprint(), streamed=streamed)

    def _attempt(self, conn: Connection, run, request, download_id: int, status, outcome: str,
                 started: float, error) -> int:
        notice, version, part = run.attachment or (None, None, None)
        with conn.begin():
            return conn.execute(text("""
                INSERT INTO acq_fetch_attempts
                    (source_id, run_id, download_id, fingerprint, method, url, status, outcome, duration_ms, error,
                     attachment_notice_key, attachment_version, attachment_part_key)
                VALUES (:s, :run, :download, :f, :m, :u, :st, :o, :d, :e, :notice, :version, :part) RETURNING id"""),
                {"s": run.report.source_id, "run": run.id, "download": download_id, "f": request.fingerprint(),
                 "m": request.method, "u": _attempt_url(request.url), "st": status, "o": outcome,
                 "notice": notice, "version": version, "part": part,
                 "d": max(0, int((self.clock.monotonic() - started) * 1000)), "e": error}).scalar_one()

    # ---- outcomes -------------------------------------------------------------------------------------------
    def _failed(self, src: DeclaredSource, url: str, failure: _Failure, run: _Run) -> None:
        url = _attempt_url(url)
        with self._write(run) as conn:
            self._failed_event(conn, src, url, failure)
        run.report.failed += 1
        run.report.failures.append({"url": url, "error_code": failure.code, "retryable": failure.retryable})

    def _block(self, src: DeclaredSource, code: str, run: _Run) -> None:
        with self._write(run) as conn:
            conn.execute(text("UPDATE acq_sources SET state = 'blocked', state_reason = :r, updated_at = now() "
                              "WHERE source_id = :s"), {"s": src.source_id, "r": code})
            self._failed_event(conn, src, _attempt_url(run.last_url), _Failure(code, False))
        run.report.blocked = code
        if run.position is not None:
            self._gap(run, *run.position, code)

    def _health(self, src: DeclaredSource, run: _Run) -> None:
        """Health from the run: blocked or nothing succeeded → unavailable; some failures → degraded; else healthy.
        A change of state is published as source.health_changed in the same transaction."""
        report = run.report
        succeeded = report.pages + report.archived + report.unchanged + report.skipped
        if report.blocked:
            state, reason = "unavailable", report.blocked
        elif report.deferred:
            state, reason = ("degraded" if succeeded else "unavailable"), report.deferred
        elif report.truncated or (report.gaps and report.failed == 0):
            state, reason = ("degraded" if succeeded else "unavailable"), "incomplete_window"
        elif report.failed == 0:
            state, reason = "healthy", "ok"
        elif succeeded:
            state, reason = "degraded", "partial_failures"
        else:
            state, reason = "unavailable", "all_failed"
        now = self.clock.now()
        recoverable = self.spool is not None or callable(getattr(src.handler, "group_request", None))
        awaiting_recovery = report.deferred or (recoverable and report.truncated)
        summary = {"pages": report.pages, "listed": report.listed, "archived": report.archived,
                   "pending_identity": report.pending_identity,
                   "unchanged": report.unchanged, "skipped": report.skipped, "failed": report.failed,
                   "truncated": report.truncated, "gaps": report.gaps, "blocked": report.blocked, "failures": report.failures}
        with run.lease.writable(self.db) as conn:
            summary["deferred"] = report.deferred
            if (run.lease.mode == "backfill" and report.gaps and not any(
                    (report.blocked, report.deferred, report.failed, report.truncated))
                    and initial_backfill.waits_for_source_change({gap["reason"] for gap in report.gaps})):
                summary["wait_for_source_change"] = True
            encoded = json.dumps(summary)
            run_summary = encoded
            if run.historical_scans:
                for gap in report.gaps:
                    if gap["reason"] == "recovered_group_requires_rescan":
                        scan = run.historical_scans.get(gap["channel"])
                        if scan is not None:
                            scan.end = None
                run_summary = json.dumps({
                    **summary, "historical_scan": {"generation": run.lease.generation,
                        "channels": {channel: scan.as_report() for channel, scan in run.historical_scans.items()}},
                })
            conn.execute(text("""
                UPDATE acq_collection_runs
                SET finished_at = :finished, report = CAST(:report AS jsonb), pending_report = CAST(:pending AS jsonb)
                WHERE id = :id
            """), {"finished": None if awaiting_recovery else now, "report": None if awaiting_recovery else run_summary,
                   "pending": run_summary if awaiting_recovery else None, "id": run.id})
            window = run.lease.window
            incomplete = any((report.deferred, report.failed, report.gaps, report.truncated))
            manual_complete = run.lease.mode == "manual" and report.pages > 0 and not incomplete
            if (run.lease.mode == "backfill" or manual_complete) and window.since == window.until and not report.blocked:
                unverified = initial_backfill.settle(conn, src.source_id, window.since, run.id,
                                                     src.handler.channels, incomplete=incomplete)
                for channel in unverified:
                    scan = run.historical_scans.get(channel)
                    self._gap(run, channel, window, max(1, len(scan.pages)) if scan else 1, "coverage_unverified")
                if unverified:
                    state, reason = "degraded", "incomplete_window"
                    conn.execute(text("""
                        UPDATE acq_collection_runs SET report = jsonb_set(report, '{gaps}', CAST(:gaps AS jsonb))
                        WHERE id = :id
                    """), {"id": run.id, "gaps": json.dumps(report.gaps)})
            if run.lease.mode in ("incremental", "backfill"):
                plans.settle(conn, run.id, pending=bool(awaiting_recovery))
            before = conn.execute(text("SELECT state FROM acq_source_health WHERE source_id = :s FOR UPDATE"),
                                  {"s": src.source_id}).scalar_one_or_none()
            conn.execute(text("""
                INSERT INTO acq_source_health (source_id, state, reason_code, checked_at, last_success_at, last_run)
                VALUES (:s, :st, :r, :t, CASE WHEN :ok THEN CAST(:t AS timestamptz) END, CAST(:run AS jsonb))
                ON CONFLICT (source_id) DO UPDATE SET state = EXCLUDED.state, reason_code = EXCLUDED.reason_code,
                    checked_at = EXCLUDED.checked_at, last_run = EXCLUDED.last_run,
                    last_success_at = COALESCE(EXCLUDED.last_success_at, acq_source_health.last_success_at)"""),
                {"s": src.source_id, "st": state, "r": reason, "t": now, "ok": succeeded > 0, "run": json.dumps(summary)})
            if before != state:
                writer.append(conn, outbox.Event(
                    type=HEALTH_CHANGED, subject=src.source_id, partition_key=src.source_id,
                    data={"source_id": src.source_id, "before": before or "unknown", "after": state, "reason_code": reason}))
            run.lease.release(conn)
        report.health = state

    def _failed_event(self, conn: Connection, src: DeclaredSource, url: str, failure: _Failure) -> None:
        writer.append(conn, outbox.Event(
            type=FAILED, subject=src.source_id, partition_key=src.source_id,
            data={"source_id": src.source_id, "source_url": url, "failed_at": _instant(self.clock.now()),
                  "error_code": failure.code, "retryable": failure.retryable}))


def _target_digest(target: tuple[tuple[str, str, int], str, str]) -> str:
    return hashlib.sha256(json.dumps(target, separators=(",", ":"), ensure_ascii=False).encode()).hexdigest()


def _attempt_url(url: str) -> str:
    try:
        request_origin(url)
        target = urlsplit(url)
    except TransportError:
        return "urn:miaoqia:invalid-collection-target"
    return target._replace(query="", fragment="").geturl()


def _retry_after(value: str | None, now: datetime) -> float:
    if not value:
        return 0
    value = value.strip()
    if value.isascii() and value.isdecimal():
        # RFC delta-seconds overflow is represented as 2**31 - 1.
        digits = value.lstrip("0") or "0"
        return min(int(digits), 2**31 - 1) if len(digits) <= 10 else 2**31 - 1
    try:
        when = parsedate_to_datetime(value)
        return max(0, (when - now).total_seconds()) if when.tzinfo is not None else 0
    except (ValueError, TypeError, OverflowError):
        return 0


def _instant(t: datetime) -> str:
    return t.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
