"""Bounded, in-memory engineering checks against one declared public source channel."""
from __future__ import annotations

import hashlib
from collections.abc import Callable, Iterable
from dataclasses import asdict, dataclass, field, replace
from datetime import timezone
from typing import Literal
from urllib.parse import parse_qsl, urlsplit

from miaoqia.kernel import jsoncodec

from ..api.handlers import (DetailRef, FetchRequest, FetchResponse, NoticeGroup, SourceBlocked, Window,
                            checked_detail_ref, resolve_detail_request)
from ..infrastructure.transport import REDIRECTS, TransportError, redirect_request, request_origin
from .discovery import ListPlanError, ListingTraversal, listing_request, matches_list_target
from .fetch import MAX_REDIRECTS, Clock, Transport
from .grouped_observations import validate_group
from .sources import CatalogSource, DeclaredSource

MAX_PAGES = 5
MAX_DETAILS = 5
_RUN_SECONDS = 180
_PUBLIC_HEADERS = {"accept", "accept-language", "content-type", "origin", "referer"}
_CREDENTIAL_PARAMETERS = {
    "accesskeyid", "accesstoken", "apikey", "auth", "authcode", "authorization", "authtoken", "bearertoken",
    "clientassertion", "clientsecret", "credential", "idtoken", "jwt", "keysecret", "password", "refreshtoken",
    "secret", "secretaccesskey", "sessionid", "sessiontoken", "sign", "signature", "token", "xamzcredential",
    "xamzsecuritytoken", "xamzsignature", "xapikey", "xgoogcredential", "xgoogsignature",
}

Outcome = Literal["validated", "limited", "blocked", "failed"]


@dataclass
class ValidationReport:
    source_id: str
    admission: str
    handler: str
    channel: str
    since: str
    until: str
    observed_at: str
    outcome: Outcome = "validated"
    pagination: Literal["not_observed", "first_page_only", "advanced"] = "not_observed"
    pages: list[dict] = field(default_factory=list)
    details: list[dict] = field(default_factory=list)
    issues: list[dict] = field(default_factory=list)
    requests: list[dict] = field(default_factory=list)

    def limit(self, stage: str, code: str, page: int) -> None:
        if self.outcome == "validated":
            self.outcome = "limited"
        self.issues.append({"stage": stage, "code": code, "page": page})


class _Stop(Exception):
    def __init__(self, code: str, outcome: Outcome = "failed"):
        super().__init__(code)
        self.code, self.outcome = code, outcome


def validate(
    entry: CatalogSource, source: DeclaredSource, channel: str, window: Window, *,
    max_pages: int, max_details: int, transport_factory: Callable[[], Transport], clock: Clock,
) -> ValidationReport:
    if channel not in source.handler.channels:
        raise ValueError(f"unknown channel {channel!r}; declared: {', '.join(source.handler.channels)}")
    if source.handler.windowed and window.since != window.until:
        raise ValueError("a windowed source requires a single publication date for validation")
    if not (1 <= max_pages <= MAX_PAGES and 1 <= max_details <= MAX_DETAILS):
        raise ValueError(f"validation budgets must be 1-{MAX_PAGES} pages and 1-{MAX_DETAILS} details")
    report = ValidationReport(
        source.source_id, entry.admission, source.handler_ref, channel, window.since.isoformat(),
        window.until.isoformat(), clock.now().astimezone(timezone.utc).isoformat(),
    )
    run = _Validation(source, report, transport_factory(), clock)
    run.execute(window, max_pages, max_details)
    return report


class _Validation:
    def __init__(self, source: DeclaredSource, report: ValidationReport, transport: Transport, clock: Clock):
        self.source = source
        self.report = report
        self.transport = transport
        self.clock = clock
        self.next_request = clock.monotonic()
        self.deadline = self.next_request + _RUN_SECONDS
        self.origin: tuple[str, str, int] | None = None
        self.stage, self.page = "list", 1

    def execute(self, window: Window, max_pages: int, max_details: int) -> None:
        try:
            samples = self._discover(window, max_pages, max_details)
            if not samples:
                self.report.limit("detail", "no_detail_sample", 1)
            self.stage = "detail"
            for self.page, sample in samples:
                try:
                    if isinstance(sample, NoticeGroup):
                        self._check_group(sample, window)
                    else:
                        self._check_detail(sample, window)
                except _Stop as exc:
                    if exc.code not in ("network_error", "http_408", "http_500", "http_502", "http_503", "http_504"):
                        raise
                    self._record_issue(exc.outcome, exc.code)
                except (ValueError, TypeError, KeyError, IndexError):
                    failure = "invalid_group_content" if isinstance(sample, NoticeGroup) else "invalid_content"
                    self._record_issue("failed", failure)
        except SourceBlocked as exc:
            self._record_issue("blocked", exc.code)
        except _Stop as exc:
            self._record_issue(exc.outcome, exc.code)
        except ListPlanError as exc:
            outcome = "limited" if exc.code == "target_outside_channel" else "failed"
            self._record_issue(outcome, exc.code)
        except (ValueError, TypeError, KeyError, IndexError):
            self._record_issue("failed", "invalid_list")

    def _record_issue(self, outcome: Outcome, code: str) -> None:
        if outcome == "limited":
            self.report.limit(self.stage, code, self.page)
            return
        self.report.outcome = outcome
        self.report.issues.append({"stage": self.stage, "code": code, "page": self.page})

    def _discover(self, window: Window, max_pages: int, max_details: int) -> list[tuple[int, DetailRef | NoticeGroup]]:
        handler, report = self.source.handler, self.report
        grouped = callable(getattr(handler, "expand_group", None))
        page_requests = set()
        traversal = ListingTraversal()
        group_requests = {}
        samples = {}
        for self.page in range(1, max_pages + 1):
            selected = listing_request(handler, report.channel, window, self.page, self.clock.now())
            fingerprint = selected.identity.fingerprint()
            if fingerprint in page_requests:
                report.limit("list", "repeated_list_request", self.page)
                break
            page_requests.add(fingerprint)
            if selected.preflight is not None:
                self._preflight(selected.preflight)
            request = selected.request
            guarded = selected.preflight is not None
            response, content_hash = self._send(request, pinned=guarded,
                                                target_mismatch="list_response_target_mismatch" if guarded else None)
            listing = handler.parse_list(report.channel, response).for_request(
                self.page, window=window if handler.windowed else None)
            if (listing.groups and not grouped) or (listing.refs and grouped):
                raise ValueError("source returned the wrong listing shape")
            evidence = {
                "page": self.page, "request_fingerprint": request.fingerprint(), "response_sha256": content_hash,
                "refs": len(listing.refs), "keyed_refs": sum(ref.source_key is not None for ref in listing.refs),
                "undated_refs": sum(ref.published is None for ref in listing.refs), "has_next": listing.has_next,
                "coverage_gap": listing.coverage_gap,
            }
            if grouped:
                evidence["groups"] = len(listing.groups)
            report.pages.append(evidence)
            issues = traversal.observe(listing, page=self.page)
            for issue in issues:
                report.limit("list", issue, self.page)
            if self.page > 1:
                if "repeated_page" in issues or "next_page_empty" in issues:
                    break
                report.pagination = "advanced"
            if listing.coverage_gap is not None:
                report.limit("list", listing.coverage_gap, self.page)
                if self.page == 1 and not listing.has_next:
                    report.pagination = "first_page_only"
            if any(ref.source_key is None for ref in listing.refs):
                report.limit("list", "identity_pending", self.page)
            for group in listing.groups:
                fingerprint = group.request.fingerprint()
                if group_requests.setdefault(group.group_key, fingerprint) != fingerprint:
                    raise ValueError("one group has conflicting publisher requests")
                if len(samples) < max_details:
                    samples.setdefault(group.group_key, (self.page, group))
            for ref in listing.refs:
                if len(samples) < max_details and (ref.published is None or window.since <= ref.published <= window.until):
                    samples.setdefault(ref.source_key or ref.source_url, (self.page, ref))
            if not listing.has_next:
                break
            if not listing.refs and not listing.groups:
                report.limit("list", "empty_list_with_next", self.page)
                break
        else:
            report.limit("list", "page_limit", max_pages + 1)
        return list(samples.values())

    def _preflight(self, request: FetchRequest) -> None:
        response, _ = self._send(request, pinned=True, target_mismatch="list_response_target_mismatch", phase="preflight")
        try:
            self.source.handler.check_list_preflight(response)
        except (ValueError, TypeError, KeyError, IndexError) as exc:
            raise _Stop("invalid_list_preflight") from exc

    def _check_detail(self, ref: DetailRef, window: Window) -> None:
        handler, report = self.source.handler, self.report
        bodies = [part for part in handler.detail_fetches(report.channel, ref) if part.kind == "body"]
        if not bodies:
            report.limit("detail", "detail_unavailable", self.page)
            return
        if len(bodies) != 1:
            raise _Stop("unsupported_detail_parts")
        part = bodies[0]
        request = part.request
        target_mismatch = None
        if part.locator is not None:
            target_mismatch = "detail_response_target_mismatch"
            location, _ = self._send(part.locator, pinned=True, target_mismatch=target_mismatch, phase="locator")
            try:
                request = resolve_detail_request(handler, report.channel, ref, location)
            except (ValueError, TypeError, KeyError, IndexError):
                self._record_issue("failed", "invalid_detail_locator")
                return
        assert request is not None
        response, content_hash = self._send(request, pinned=True, target_mismatch=target_mismatch)
        ref = checked_detail_ref(handler, ref, response)
        if ref.published is not None and not window.since <= ref.published <= window.until:
            report.limit("detail", "detail_outside_window", self.page)
        report.details.append({
            "page": self.page, "source_key": ref.source_key, "artifact_key": part.artifact_key,
            "published": ref.published.isoformat() if ref.published is not None else None,
            "request_fingerprint": request.fingerprint(), "response_sha256": content_hash,
            "media_type": response.media_type,
        })

    def _check_group(self, group: NoticeGroup, window: Window) -> None:
        handler, report = self.source.handler, self.report
        request = group.request
        response, content_hash = self._send(request, pinned=True,
                                            target_mismatch="group_response_target_mismatch", phase="group")
        capture = validate_group(handler, report.channel, group, response)
        members = []
        for member in capture.members:
            ref = member.ref
            if window.since <= ref.published <= window.until:
                members.append({"source_key": ref.source_key, "published": ref.published.isoformat(),
                                "selector": asdict(member.selector), "record_digest": member.record_digest})
        if not members:
            report.limit("detail", "no_group_notice_in_window", self.page)
        report.details.append({"page": self.page, "group_key": group.group_key, "notice_count": len(capture.members),
                               "notices": members, "request_fingerprint": request.fingerprint(),
                               "response_sha256": content_hash, "media_type": response.media_type,
                               "size_bytes": len(response.body)})

    def _send(self, request: FetchRequest, *, pinned: bool = False, target_mismatch: str | None = None,
              phase: str | None = None) -> tuple[FetchResponse, str]:
        try:
            origin = request_origin(request.url)
        except TransportError as exc:
            raise _Stop("invalid_target") from exc
        if self.origin is None:
            self.origin = origin
        if origin != self.origin:
            raise _Stop("target_outside_channel", "limited")
        headers = self.source.access.headers + request.headers
        if any(name.lower() not in _PUBLIC_HEADERS for name, _ in headers):
            raise _Stop("non_public_request_header", "limited")
        request = replace(request, headers=headers)
        for hop in range(MAX_REDIRECTS + 1):
            response = self._send_hop(request, phase=phase)
            if response.status in (401, 403):
                raise SourceBlocked(f"http_{response.status}")
            if response.status == 429 or (target_mismatch is not None and response.retry_after is not None):
                raise SourceBlocked("throttled")
            if response.status == 200:
                if target_mismatch is not None and not matches_list_target(request, response):
                    raise _Stop(target_mismatch)
                content_hash = _sha256(response.body)
                self.report.requests[-1]["response_sha256"] = content_hash
                return response, content_hash
            if response.status not in REDIRECTS:
                raise _Stop(f"http_{response.status}")
            if pinned:
                raise _Stop("part_redirect_disallowed", "limited")
            if hop == MAX_REDIRECTS:
                raise _Stop("redirect_limit", "limited")
            try:
                request = redirect_request(request, response)
            except TransportError as exc:
                raise _Stop("redirect_disallowed", "limited") from exc
        raise AssertionError("unreachable redirect state")

    def _send_hop(self, request: FetchRequest, *, phase: str | None) -> FetchResponse:
        _check_credential_fields(parse_qsl(urlsplit(request.url).query))
        _check_credential_fields(request.form)
        _check_json_body(request.json_body)
        if max(self.next_request, self.clock.monotonic()) >= self.deadline:
            raise _Stop("time_limit", "limited")
        delay = self.next_request - self.clock.monotonic()
        if delay > 0:
            self.clock.sleep(delay)
        now = self.clock.monotonic()
        if now >= self.deadline:
            raise _Stop("time_limit", "limited")
        access = self.source.access
        self.next_request = now + access.min_interval_seconds
        evidence = {
            "stage": self.stage, "page": self.page, "method": request.method,
            "request_fingerprint": request.fingerprint(), "status": None,
        }
        if phase is not None:
            evidence["phase"] = phase
        self.report.requests.append(evidence)
        try:
            response = self.transport.send(request, min(access.timeout_seconds, self.deadline - now))
        except TransportError as exc:
            raise _Stop("network_error") from exc
        evidence["status"] = response.status
        if self.clock.monotonic() >= self.deadline:
            raise _Stop("time_limit", "limited")
        return response


def _sha256(body: bytes) -> str:
    return "sha256:" + hashlib.sha256(body).hexdigest()


def _check_credential_fields(parameters: Iterable[tuple[str, object]]) -> None:
    for name, value in parameters:
        normalized = "".join(char for char in name.lower() if char.isalnum())
        if normalized in _CREDENTIAL_PARAMETERS and (not isinstance(value, str) or value.strip()):
            raise _Stop("credential_parameter_disallowed", "limited")


def _check_json_body(body: str | None) -> None:
    if body is None:
        return
    try:
        if not isinstance(body, str):
            raise ValueError("JSON body must be text")
        payload = jsoncodec.loads(body.encode("utf-8"))
        if not isinstance(payload, dict):
            raise ValueError("JSON body must be an object")
    except (ValueError, RecursionError) as exc:
        raise _Stop("invalid_json_request") from exc
    _check_json_credentials(payload)


def _check_json_credentials(value: object) -> None:
    if isinstance(value, dict):
        _check_credential_fields(value.items())
        children = value.values()
    elif isinstance(value, list):
        children = value
    else:
        return
    for child in children:
        _check_json_credentials(child)
