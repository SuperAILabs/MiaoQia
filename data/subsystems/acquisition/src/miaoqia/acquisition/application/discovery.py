"""Persist listing receipts and retire only the expiring response bytes."""
from __future__ import annotations

import hashlib
import json
import time
from collections.abc import Callable
from contextlib import AbstractContextManager
from dataclasses import dataclass
from datetime import date, datetime, timedelta
from urllib.parse import urlsplit

from sqlalchemy import Connection, text

from miaoqia.kernel import db, reqctx
from miaoqia.kernel.objectstore import ObjectConflict, ObjectStore

from ..api import NAME
from ..api.handlers import FetchRequest, FetchResponse, ListPage, ListRequestPlan, SourceHandler, Window
from ..infrastructure.transport import TransportError, request_origin


class ListPlanError(ValueError):
    def __init__(self, code: str, url: str = ""):
        super().__init__(code)
        self.code, self.url = code, url


@dataclass(frozen=True)
class ListingRequest:
    identity: FetchRequest
    request: FetchRequest
    origin: tuple[str, str, int]
    preflight: FetchRequest | None = None


def listing_request(handler: SourceHandler, channel: str, window: Window, page: int,
                    now: datetime) -> ListingRequest:
    plan_builder = getattr(handler, "list_plan", None)
    if plan_builder is None:
        request = handler.list_request(channel, window, page)
        return ListingRequest(request, request, _list_origin(request))
    if not callable(plan_builder):
        raise ListPlanError("invalid_list_plan")
    try:
        plan = plan_builder(channel, window, page, now)
    except (ValueError, TypeError, KeyError, IndexError) as exc:
        raise ListPlanError("invalid_list_plan") from exc
    if (not isinstance(plan, ListRequestPlan) or not callable(getattr(handler, "check_list_preflight", None))
            or not all(isinstance(item, FetchRequest) for item in (plan.identity, plan.request, plan.preflight))):
        raise ListPlanError("invalid_list_plan")
    origin = _list_origin(plan.identity)
    for request in (plan.request, plan.preflight):
        if _list_origin(request) != origin:
            raise ListPlanError("target_outside_channel", request.url)
    if (plan.identity.method, plan.identity.url) != (plan.request.method, plan.request.url):
        raise ListPlanError("invalid_list_plan", plan.request.url)
    return ListingRequest(plan.identity, plan.request, origin, plan.preflight)


def _list_origin(request: FetchRequest) -> tuple[str, str, int]:
    try:
        return request_origin(request.url)
    except TransportError as exc:
        raise ListPlanError("invalid_target", request.url) from exc


def request_target(url: str) -> tuple[tuple[str, str, int], str, str]:
    origin = request_origin(url)
    target = urlsplit(url)
    return origin, target.path or "/", target.query


def matches_list_target(request: FetchRequest, response: FetchResponse) -> bool:
    try:
        return request_target(request.url) == request_target(response.url)
    except TransportError:
        return False


class ListingTraversal:
    """Check sequence-wide coverage without discarding admissible original bytes."""

    def __init__(self):
        self.members: set[str] = set()
        self.ordered = True
        self.lowest_publication: date | None = None
        self.offset_total: int | None = None

    def observe(self, listing: ListPage, *, page: int) -> list[str]:
        issues = []
        if listing.groups:
            members = {group.group_key for group in listing.groups}
            if len(members) != len(listing.groups):
                issues.append("duplicate_group_in_page")
        else:
            members = {ref.source_key or ref.source_url for ref in listing.refs}
            if len(members) != len(listing.refs):
                issues.append("duplicate_notice_in_page")
        if members and members <= self.members:
            issues.append("repeated_page")
        elif members & self.members:
            issues.append("overlapping_page")
        if not members and page > 1:
            issues.append("next_page_empty")
        self.members.update(members)

        if listing.offset is not None:
            total = listing.offset.total_records
            if self.offset_total is not None and total != self.offset_total:
                issues.append("offset_total_changed")
                self.ordered = False
            self.offset_total = total

        if not listing.ordered_by_publication:
            self.ordered = False
        elif listing.refs:
            dates = [ref.published for ref in listing.refs if ref.published is not None]
            if (len(dates) != len(listing.refs) or dates != sorted(dates, reverse=True)
                    or (self.lowest_publication is not None and dates[0] > self.lowest_publication)):
                self.ordered = False
                issues.append("inconsistent_publication_order")
            else:
                self.lowest_publication = dates[-1]
        return issues

    def past_window(self, window: Window) -> bool:
        return self.ordered and self.lowest_publication is not None and self.lowest_publication < window.since


def record(write: Callable[[], AbstractContextManager[Connection]], store: ObjectStore, *, source: str, run: int, attempt: int, channel: str,
           window: Window, page: int, response: FetchResponse, listing: ListPage, retained_at: datetime,
           days: int, require_storage: bool = False, group_task_keys: tuple[str, ...] = (),
           browser_binding: dict | None = None) -> bool:
    if len(group_task_keys) != len(listing.groups):
        raise ValueError("discovery must record every group task")
    digest = hashlib.sha256(response.body).hexdigest()
    object_ref, expiry, kept = None, None, True
    if days:
        key = f"discovery/{source}/{attempt}/{digest}"
        try:
            store.put(key, response.body)
            if require_storage and store.get(key, max_bytes=len(response.body)) != response.body:
                raise ObjectConflict(key)
        except (OSError, ObjectConflict, ValueError):
            if require_storage:
                return False
            kept = False
        else:
            object_ref, expiry = key, retained_at + timedelta(days=days)
    refs = [{"notice_key": ref.source_key, "source_url": ref.source_url,
             "published_on": ref.published.isoformat() if ref.published else None} for ref in listing.refs]
    groups = [{"group_key": group.group_key, "request_fingerprint": group.request.fingerprint()}
              for group in listing.groups]
    with write() as conn:
        conn.execute(text("""
            INSERT INTO acq_discovery_pages (attempt_id, run_id, channel, window_start, window_end, page,
                response_hash, refs, has_next, object_ref, expires_at, browser_binding)
            VALUES (:attempt, :run, :channel, :start, :end, :page, :hash, CAST(:refs AS jsonb), :next, :obj, :expires,
                    CAST(:browser AS jsonb))
            ON CONFLICT (attempt_id) DO NOTHING
        """), {"attempt": attempt, "run": run, "channel": channel, "start": window.since, "end": window.until,
               "page": page, "hash": f"sha256:{digest}", "refs": json.dumps(refs or groups), "next": listing.has_next,
               "obj": object_ref, "expires": expiry,
               "browser": json.dumps(browser_binding) if browser_binding is not None else None})
        if groups:
            conn.execute(text("""
                INSERT INTO acq_request_tasks (task_key, run_id, kind, scope, fingerprint)
                VALUES (:task, :run, 'group', CAST(:scope AS jsonb), :fingerprint)
                ON CONFLICT (task_key) DO NOTHING
            """), [{"task": task_key, "run": run, "fingerprint": group["request_fingerprint"],
                    "scope": json.dumps([channel, window.since.isoformat(), window.until.isoformat(),
                                         group["group_key"], attempt, page])}
                   for task_key, group in zip(group_task_keys, groups, strict=True)])
    return kept


def expire(database: db.Database, store: ObjectStore, *, deadline: float) -> int:
    expired = 0
    with reqctx.use(reqctx.service(NAME)):
        while expired < 100 and time.monotonic() < deadline:
            with database.tx() as conn:
                capture = conn.execute(text("""
                    SELECT attempt_id, object_ref FROM acq_discovery_pages
                    WHERE expires_at <= now() AND expired_at IS NULL
                    ORDER BY expires_at, attempt_id LIMIT 1 FOR UPDATE SKIP LOCKED
                """)).first()
                if capture is None:
                    break
                store.delete_discovery(capture.object_ref)
                conn.execute(text("UPDATE acq_discovery_pages SET expired_at = now() WHERE attempt_id = :id"),
                             {"id": capture.attempt_id})
            expired += 1
    return expired
