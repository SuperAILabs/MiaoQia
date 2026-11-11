"""Fetch runtime: list discovery, detail fetch, rate limiting, retry and backoff, dedup, archive and outbox events.

One run collects one source over a publication window. Every request goes through the per-source limiter and is
recorded in acq_fetch_attempts. A detail request already archived is skipped unless refetch is requested; refetched
content identical to an archived artifact produces no new artifact or event. A source that answers with a captcha,
login wall or 401/403 is marked blocked and stays stopped until an operator unblocks it; the runtime never tries to
get around it (acquisition.md section 9).

Object store writes precede the database transaction: objects are immutable and content-addressed, so a rolled back
transaction leaves only an unreferenced object.
"""
from __future__ import annotations

import hashlib
import json
import time
from dataclasses import dataclass, field
from datetime import date, datetime, timedelta, timezone
from typing import Protocol

from sqlalchemy import Connection, text

from miaoqia.kernel import db, outbox, reqctx
from miaoqia.kernel.objectstore import ObjectStore

from ..api import NAME
from ..api.handlers import DetailFetch, DetailRef, FetchRequest, FetchResponse, SourceBlocked, Window
from ..infrastructure.transport import TransportError
from .sources import DeclaredSource

COMPLETED = "acquisition.fetch.completed.v1"
FAILED = "acquisition.fetch.failed.v1"
HEALTH_CHANGED = "acquisition.source.health_changed.v1"
CHINA = timezone(timedelta(hours=8))


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


@dataclass
class RunReport:
    source_id: str
    pages: int = 0
    listed: int = 0
    archived: int = 0
    unchanged: int = 0
    skipped: int = 0
    failed: int = 0
    blocked: str | None = None
    truncated: bool = False                      # the run reached its deadline before finishing
    health: str | None = None
    failures: list[dict] = field(default_factory=list)


@dataclass(frozen=True)
class _Failure:
    code: str
    retryable: bool


class Fetcher:
    def __init__(self, database: db.Database, store: ObjectStore, transport: Transport, *, clock: Clock | None = None):
        self.db = database
        self.store = store
        self.transport = transport
        self.clock = clock or SystemClock()
        self._last: dict[str, float] = {}
        self._last_url: dict[str, str] = {}
        self._deadline: float | None = None

    # ---- registry -------------------------------------------------------------------------------------------
    def register(self, conn: Connection, src: DeclaredSource) -> str:
        """Upsert the declared handler and access contract; runtime state is kept. Returns the state."""
        return conn.execute(text("""
            INSERT INTO acq_sources (source_id, handler, access) VALUES (:s, :h, CAST(:a AS jsonb))
            ON CONFLICT (source_id) DO UPDATE SET handler = EXCLUDED.handler, access = EXCLUDED.access,
                updated_at = CASE WHEN acq_sources.handler IS DISTINCT FROM EXCLUDED.handler
                                    OR acq_sources.access IS DISTINCT FROM EXCLUDED.access
                                  THEN now() ELSE acq_sources.updated_at END
            RETURNING state"""), {"s": src.source_id, "h": src.handler_ref, "a": json.dumps(src.access_json())}).scalar_one()

    # ---- run ------------------------------------------------------------------------------------------------
    def run(self, src: DeclaredSource, window: Window, *, max_pages: int, refetch: bool = False,
            unblock: bool = False, deadline: float | None = None) -> RunReport:
        """Collect one source. deadline (clock.monotonic()) stops the run between requests; health is updated last."""
        if max_pages < 1:
            raise ValueError("max_pages must be positive")
        with reqctx.use(reqctx.service(NAME)):
            with self.db.tx() as conn:
                state = self.register(conn, src)
                if state == "blocked":
                    if not unblock:
                        raise SourceStopped(src.source_id)
                    conn.execute(text("UPDATE acq_sources SET state = 'active', state_reason = NULL, updated_at = now() "
                                      "WHERE source_id = :s"), {"s": src.source_id})
            report = RunReport(src.source_id)
            self._deadline = deadline
            try:
                for channel in src.handler.channels:
                    for w in self._windows(src, window):
                        if self._expired(report):
                            break
                        self._channel(src, channel, w, window, max_pages, refetch, report)
            except SourceBlocked as blocked:
                self._block(src, blocked.code, report)
            self._health(src, report)
            return report

    def _expired(self, report: RunReport) -> bool:
        if self._deadline is not None and self.clock.monotonic() >= self._deadline:
            report.truncated = True
        return report.truncated

    def _windows(self, src: DeclaredSource, window: Window) -> list[Window]:
        if not src.handler.windowed:
            return [window]
        days = (window.until - window.since).days
        return [Window(d, d) for d in (window.until - timedelta(n) for n in range(days + 1))]

    def _channel(self, src, channel, w, window, max_pages, refetch, report) -> None:
        handler = src.handler
        for page in range(1, max_pages + 1):
            if self._expired(report):
                return
            request = handler.list_request(channel, w, page)
            resp = self._send(src, request)
            if isinstance(resp, _Failure):
                self._failed(src, request.url, resp, report)
                return
            try:
                listing = handler.parse_list(channel, resp)
            except ValueError:
                self._failed(src, request.url, _Failure("invalid_list", False), report)
                return
            report.pages += 1
            older = False
            for ref in listing.refs:
                if ref.published is not None and ref.published > window.until:
                    continue
                if ref.published is not None and ref.published < window.since:
                    older = True
                    continue
                report.listed += 1
                for fetch in handler.detail_fetches(channel, ref):
                    if self._expired(report):
                        return
                    self._detail(src, ref, fetch, refetch, report)
            if not listing.has_next or older or not listing.refs:
                return

    def _detail(self, src: DeclaredSource, ref: DetailRef, fetch: DetailFetch, refetch: bool, report: RunReport) -> None:
        fp = fetch.request.fingerprint()
        if not refetch:
            with self.db.tx() as conn:
                if conn.execute(text("SELECT 1 FROM acq_requests WHERE fingerprint = :f"), {"f": fp}).first():
                    report.skipped += 1
                    return
        resp = self._send(src, fetch.request)
        if isinstance(resp, _Failure):
            self._failed(src, ref.source_url, resp, report)
            return
        try:
            src.handler.check_detail(resp)
        except ValueError:
            self._failed(src, ref.source_url, _Failure("invalid_content", False), report)
            return
        digest = hashlib.sha256(resp.body).hexdigest()
        content_hash, artifact_ref = f"sha256:{digest}", f"raw/{digest}"
        self.store.put(artifact_ref, resp.body)
        fetched_at = self.clock.now()
        with self.db.tx() as conn:
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": f"{src.source_id}|{fetch.artifact_key}"})
            conn.execute(text("""
                INSERT INTO acq_requests (fingerprint, source_id, source_key, content_hash, last_fetched_at)
                VALUES (:f, :s, :k, :h, :t)
                ON CONFLICT (fingerprint) DO UPDATE SET content_hash = EXCLUDED.content_hash, last_fetched_at = EXCLUDED.last_fetched_at"""),
                {"f": fp, "s": src.source_id, "k": fetch.artifact_key, "h": content_hash, "t": fetched_at})
            inserted = conn.execute(text("""
                INSERT INTO acq_raw_artifacts (source_id, source_key, content_hash, artifact_ref, media_type, size_bytes,
                                               source_url, request_fingerprint, fetched_at)
                VALUES (:s, :k, :h, :a, :m, :n, :u, :f, :t)
                ON CONFLICT (source_id, source_key, content_hash) DO NOTHING RETURNING id"""),
                {"s": src.source_id, "k": fetch.artifact_key, "h": content_hash, "a": artifact_ref, "m": resp.media_type,
                 "n": len(resp.body), "u": ref.source_url, "f": fp, "t": fetched_at}).first()
            if inserted is None:
                report.unchanged += 1
                return
            writer.append(conn, outbox.Event(
                type=COMPLETED, subject=artifact_ref, partition_key=f"{src.source_id}|{fetch.artifact_key}",
                data={"source_id": src.source_id, "artifact_ref": artifact_ref, "rights_tags": rights_tags(src.source_id),
                      "content_hash": content_hash, "fetched_at": _instant(fetched_at), "source_url": ref.source_url}))
        report.archived += 1

    # ---- requests -------------------------------------------------------------------------------------------
    def _send(self, src: DeclaredSource, request: FetchRequest) -> FetchResponse | _Failure:
        access = src.access
        request = FetchRequest(request.method, request.url, request.form, request.json_body,
                               access.headers + request.headers)
        failure = _Failure("exhausted", True)
        self._last_url[src.source_id] = request.url
        for attempt in range(1, access.max_attempts + 1):
            self._pace(src)
            started = self.clock.monotonic()
            try:
                resp = self.transport.send(request, access.timeout_seconds)
            except TransportError as e:
                self._attempt(src, request, None, "network_error", started, str(e))
                failure = _Failure("network_error", True)
                self._wait(access.min_interval_seconds * 2 ** attempt)
                continue
            if resp.status == 429:
                self._attempt(src, request, resp.status, "throttled", started, None)
                failure = _Failure("throttled", True)
                self._wait(access.throttle_backoff_seconds * 2 ** (attempt - 1))
                continue
            if resp.status in (401, 403):
                self._attempt(src, request, resp.status, "blocked", started, None)
                raise SourceBlocked(f"http_{resp.status}")
            if resp.status >= 500:
                self._attempt(src, request, resp.status, "http_error", started, None)
                failure = _Failure(f"http_{resp.status}", True)
                self._wait(access.min_interval_seconds * 2 ** attempt)
                continue
            if resp.status != 200:
                self._attempt(src, request, resp.status, "http_error", started, None)
                return _Failure(f"http_{resp.status}", False)
            self._attempt(src, request, resp.status, "ok", started, None)
            return resp
        return failure

    def _pace(self, src: DeclaredSource) -> None:
        last = self._last.get(src.source_id)
        if last is not None:
            remaining = src.access.min_interval_seconds - (self.clock.monotonic() - last)
            if remaining > 0:
                self.clock.sleep(remaining)
        self._last[src.source_id] = self.clock.monotonic()

    def _wait(self, seconds: float) -> None:
        self.clock.sleep(seconds)

    def _attempt(self, src, request: FetchRequest, status, outcome: str, started: float, error) -> None:
        with self.db.tx() as conn:
            conn.execute(text("""
                INSERT INTO acq_fetch_attempts (source_id, fingerprint, method, url, status, outcome, duration_ms, error)
                VALUES (:s, :f, :m, :u, :st, :o, :d, :e)"""),
                {"s": src.source_id, "f": request.fingerprint(), "m": request.method, "u": request.url, "st": status,
                 "o": outcome, "d": max(0, int((self.clock.monotonic() - started) * 1000)), "e": error})

    # ---- outcomes -------------------------------------------------------------------------------------------
    def _failed(self, src: DeclaredSource, url: str, failure: _Failure, report: RunReport) -> None:
        report.failed += 1
        report.failures.append({"url": url, "error_code": failure.code, "retryable": failure.retryable})
        with self.db.tx() as conn:
            self._failed_event(conn, src, url, failure)

    def _block(self, src: DeclaredSource, code: str, report: RunReport) -> None:
        report.blocked = code
        with self.db.tx() as conn:
            conn.execute(text("UPDATE acq_sources SET state = 'blocked', state_reason = :r, updated_at = now() "
                              "WHERE source_id = :s"), {"s": src.source_id, "r": code})
            self._failed_event(conn, src, self._last_url[src.source_id], _Failure(code, False))

    def _health(self, src: DeclaredSource, report: RunReport) -> None:
        """Health from the run: blocked or nothing succeeded → unavailable; some failures → degraded; else healthy.
        A change of state is published as source.health_changed in the same transaction."""
        succeeded = report.pages + report.archived + report.unchanged + report.skipped
        if report.blocked:
            state, reason = "unavailable", report.blocked
        elif report.failed == 0:
            state, reason = "healthy", "ok"
        elif succeeded:
            state, reason = "degraded", "partial_failures"
        else:
            state, reason = "unavailable", "all_failed"
        report.health = state
        now = self.clock.now()
        run = {"pages": report.pages, "listed": report.listed, "archived": report.archived, "unchanged": report.unchanged,
               "skipped": report.skipped, "failed": report.failed, "truncated": report.truncated}
        with self.db.tx() as conn:
            before = conn.execute(text("SELECT state FROM acq_source_health WHERE source_id = :s FOR UPDATE"),
                                  {"s": src.source_id}).scalar_one_or_none()
            conn.execute(text("""
                INSERT INTO acq_source_health (source_id, state, reason_code, checked_at, last_success_at, last_run)
                VALUES (:s, :st, :r, :t, CASE WHEN :ok THEN CAST(:t AS timestamptz) END, CAST(:run AS jsonb))
                ON CONFLICT (source_id) DO UPDATE SET state = EXCLUDED.state, reason_code = EXCLUDED.reason_code,
                    checked_at = EXCLUDED.checked_at, last_run = EXCLUDED.last_run,
                    last_success_at = COALESCE(EXCLUDED.last_success_at, acq_source_health.last_success_at)"""),
                {"s": src.source_id, "st": state, "r": reason, "t": now, "ok": succeeded > 0, "run": json.dumps(run)})
            if before != state:
                writer.append(conn, outbox.Event(
                    type=HEALTH_CHANGED, subject=src.source_id, partition_key=src.source_id,
                    data={"source_id": src.source_id, "before": before or "unknown", "after": state, "reason_code": reason}))

    def _failed_event(self, conn: Connection, src: DeclaredSource, url: str, failure: _Failure) -> None:
        writer.append(conn, outbox.Event(
            type=FAILED, subject=src.source_id, partition_key=src.source_id,
            data={"source_id": src.source_id, "source_url": url, "failed_at": _instant(self.clock.now()),
                  "error_code": failure.code, "retryable": failure.retryable}))


def _instant(t: datetime) -> str:
    return t.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
