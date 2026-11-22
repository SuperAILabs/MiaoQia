"""Commit an original and a rendered projection against one browser attempt."""
from __future__ import annotations

import hashlib
import json
from contextlib import suppress
from dataclasses import dataclass
from datetime import date, datetime

from sqlalchemy import Connection, text

from miaoqia.kernel import outbox
from miaoqia.kernel.db import Database
from miaoqia.kernel.objectstore import ObjectStore

from ..api.browser_pairs import (
    BROWSER_NOTICE_ARCHIVED, describe_projection, read_browser_pair, seal_browser_event, validate_browser_event,
)
from ..api.browser_projection import build_projection
from ..api.handlers import BrowserNoticeValidator, BrowserSelection, DetailRef, FetchRequest, FetchResponse, Window
from ..infrastructure.archive_spool import ArchiveSpool, Capture
from ..infrastructure.browser_capture import BrowserCapture
from ..infrastructure.transport import request_origin
from .collection_leases import CollectionLease
from .fetch import Fetcher, _attempt_url, _instant, rights_tags, writer


class BrowserConflict(ValueError):
    """The staged capture conflicts with the task or observation history."""


@dataclass(frozen=True)
class BrowserSubmission:
    version: int
    changed: bool
    replayed: bool = False


def prepare_browser_pair(*, lease: CollectionLease, ref: DetailRef, request: FetchRequest,
                         capture: BrowserCapture, selection: BrowserSelection, validate: BrowserNoticeValidator,
                         task_key: str, attempt_id: int, list_attempt_id: int,
                         received_at: datetime) -> tuple[dict, bytes]:
    """Prepare the candidate for immutable storage after publisher-specific verification."""
    response = capture.response
    if (request.method != "GET" or capture.method != request.method or response.url != request.url
            or response.status != 200 or response.redirect_to or response.retry_after
            or (capture.frame_id, capture.loader_id) != (selection.frame_id, selection.loader_id)
            or not ref.source_key or received_at.utcoffset() is None):
        raise BrowserConflict("browser response and projection are not from the approved navigation")
    published = validate(ref, response, selection)
    if type(published) is not date or (ref.published is not None and published != ref.published):
        raise BrowserConflict("browser publication date disagrees with the listing")
    projection = build_projection(selection.html)
    digest = hashlib.sha256(response.body).hexdigest()
    pair = seal_browser_event({
        "source_id": lease.source_id, "notice_key": ref.source_key, "observation_version": 1,
        "artifact_ref": f"raw/{digest}", "content_hash": f"sha256:{digest}", "rights_tags": rights_tags(lease.source_id),
        "fetched_at": _instant(received_at), "source_url": ref.source_url, "published_on": published.isoformat(),
        "media_type": response.media_type, "size_bytes": len(response.body), "content_encoding": "identity",
        "projection": describe_projection(projection),
        "binding": {
            "version": 1, "run_id": lease.run_id, "task_key": task_key, "attempt_id": attempt_id,
            "list_attempt_id": list_attempt_id, "request_url": _attempt_url(response.url),
            "request_fingerprint": request.fingerprint(), "navigation_id": selection.navigation_id,
            "frame_id": capture.frame_id, "loader_id": capture.loader_id, "request_id": capture.request_id,
        },
    })
    return pair, projection


def store_browser_pair(store: ObjectStore, pair: dict, original: bytes, projection: bytes) -> None:
    """Write a validated checkpoint's two immutable objects, permitting exact retries."""
    _check_pair_bytes(pair, original, projection)
    store.put(pair["artifact_ref"], original)
    store.put(pair["projection"]["artifact_ref"], projection)


def _check_pair_bytes(pair: dict, original: bytes, projection: bytes) -> None:
    validate_browser_event(pair)
    if (len(original) != pair["size_bytes"] or f"sha256:{hashlib.sha256(original).hexdigest()}" != pair["content_hash"]
            or describe_projection(projection) != pair["projection"]):
        raise BrowserConflict("capture bytes do not match the checkpoint")


def archive_browser_pair(database: Database, store: ObjectStore, spool: ArchiveSpool, *,
                         lease: CollectionLease, channel: str, window: Window, checkpoint: dict,
                         captured: tuple[bytes, bytes] | None = None) -> BrowserSubmission:
    """Publish a new capture or recover the same completed pair from its task checkpoint."""
    if captured is not None:
        _check_pair_bytes(checkpoint, *captured)
    with database.tx() as conn:
        stage_browser_pair(conn, lease=lease, channel=channel, window=window, checkpoint=checkpoint)

    binding = checkpoint["binding"]
    attempt = binding["attempt_id"]
    spool_key = hashlib.sha256(f"browser/{binding['task_key']}/{attempt}".encode()).hexdigest()
    received = datetime.fromisoformat(checkpoint["fetched_at"].replace("Z", "+00:00"))
    if captured is not None:
        original, projection = captured
        response = FetchResponse(200, checkpoint["media_type"], original, binding["request_url"])
        with suppress(FileExistsError):
            spool.put(spool_key, Capture(response, attempt, received, projection))

    staged = spool.get(spool_key)
    if staged is not None:
        if (staged.attempt_id != attempt or staged.received_at != received
                or staged.response.url != binding["request_url"] or staged.response.media_type != checkpoint["media_type"]
                or staged.projection is None):
            raise BrowserConflict("browser spool capture does not match the checkpoint")
        store_browser_pair(store, checkpoint, staged.response.body, staged.projection)
    elif captured is not None:
        raise BrowserConflict("browser spool capture unavailable")

    with database.tx() as conn:
        result = submit_browser_pair(conn, store, lease=lease, channel=channel, window=window, checkpoint=checkpoint)
    with suppress(OSError):
        spool.discard(spool_key, attempt)
    return result


def stage_browser_pair(conn: Connection, *, lease: CollectionLease, channel: str, window: Window,
                       checkpoint: dict) -> None:
    """Persist the verified candidate before its two objects are written."""
    if not conn.in_transaction():
        raise RuntimeError("browser staging requires an outer transaction")
    params = _task_params(lease, channel, window, checkpoint)
    with conn.begin_nested():
        lease.check(conn)
        _check_collection_rights(conn, params)
        state = _bound_task(conn, params, staging=True)
        if state in ("staged", "settled"):
            return
        if state != "queued":
            raise BrowserConflict("browser task does not match the accepted attempt and discovery")
        conn.execute(text("""
            UPDATE acq_request_tasks SET state = 'staged', scope = CAST(:scope AS jsonb), updated_at = clock_timestamp()
            WHERE task_key = :task
        """), params)


def submit_browser_pair(conn: Connection, store: ObjectStore, *, lease: CollectionLease, channel: str,
                        window: Window, checkpoint: dict) -> BrowserSubmission:
    """Publish only from a fenced, staged browser task with both exact objects readable.

    The collector writes the sealed candidate using observation_version=1 as a
    checkpoint placeholder. The task stores [channel, since, until, notice_key,
    list_attempt_id, checkpoint] so the same pair can be recovered. The source
    task owns attempt creation and selection.
    """
    if not conn.in_transaction():
        raise RuntimeError("browser submission requires an outer transaction")
    params = _task_params(lease, channel, window, checkpoint)
    pair = checkpoint
    binding = pair["binding"]
    key = {"s": lease.source_id, "k": pair["notice_key"]}
    with conn.begin_nested():
        lease.check(conn)
        _check_collection_rights(conn, key)
        state = _bound_task(conn, params)
        if state not in ("staged", "settled"):
            raise BrowserConflict("browser task does not match the accepted attempt and discovery")
        read_browser_pair(store, pair)
        _check_collection_rights(conn, key)
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": f"{key['s']}|{key['k']}"})
        latest = Fetcher._latest(conn, key)
        if state == "settled":
            return _replay(conn, pair, params)
        if latest is not None and (
            latest.browser_attempt_id is None or (latest.run_id, latest.attempt_id) >= (lease.run_id, binding["attempt_id"])
        ):
            raise BrowserConflict("browser attempt is stale or conflicts with another capture contract")
        changed = latest is None or not _same_pair(conn, latest.browser_attempt_id, pair)
        version = (latest.version if latest else 0) + int(changed)
        fields = {name: value for name, value in pair.items() if name != "binding_digest"}
        event = seal_browser_event({**fields, "observation_version": version})
        artifact_id = Fetcher._artifact(conn, {**params, "part": f"browser:{key['k']}"})
        _insert_receipt(conn, params, artifact_id, event)
        if changed:
            _append_observation(conn, params, artifact_id, version)
            writer.append(conn, outbox.Event(type=BROWSER_NOTICE_ARCHIVED, subject=pair["artifact_ref"],
                                             partition_key=f"{key['s']}|{key['k']}", data=event))
        Fetcher._advance_check(conn, {**params, "v": version, "legacy": None})
        Fetcher._task_state(conn, binding["task_key"], "settled", binding["attempt_id"])
        return BrowserSubmission(version, changed)


def _task_params(lease, channel, window, checkpoint):
    pair = validate_browser_event(checkpoint)
    binding = pair["binding"]
    published = date.fromisoformat(pair["published_on"])
    if (pair["source_id"] != lease.source_id or binding["run_id"] != lease.run_id
            or pair["observation_version"] != 1 or pair["rights_tags"] != rights_tags(lease.source_id)
            or not lease.window.since <= window.since <= published <= window.until <= lease.window.until):
        raise BrowserConflict("browser checkpoint disagrees with its source or window")

    base_scope = [channel, window.since.isoformat(), window.until.isoformat(), pair["notice_key"],
                  binding["list_attempt_id"]]
    return {"s": lease.source_id, "k": pair["notice_key"],
              "run": lease.run_id, "task": binding["task_key"], "attempt": binding["attempt_id"],
              "list_attempt": binding["list_attempt_id"], "f": binding["request_fingerprint"],
              "request_url": binding["request_url"], "channel": channel, "p": published, "u": pair["source_url"],
              "start": window.since, "end": window.until, "h": pair["content_hash"], "a": pair["artifact_ref"],
              "m": pair["media_type"], "n": pair["size_bytes"],
              "t": datetime.fromisoformat(pair["fetched_at"].replace("Z", "+00:00")),
              "base_scope": json.dumps(base_scope), "scope": json.dumps([*base_scope, pair])}


def _check_collection_rights(conn, key):
    allowed = conn.execute(text("""
        SELECT public.acq_collection_allowed(source_id) FROM acq_sources
        WHERE source_id = :s AND state = 'active'
    """), key).scalar_one_or_none()
    if not allowed:
        raise BrowserConflict("source cannot archive a browser observation")


def _bound_task(conn, params, *, staging=False):
    row = conn.execute(text("""
        SELECT t.state, la.url AS list_url FROM acq_request_tasks t
        JOIN acq_download_starts d ON d.task_key = t.task_key AND d.run_id = t.run_id
        JOIN acq_fetch_attempts a ON a.download_id = d.id AND a.id = t.last_attempt
        JOIN acq_discovery_pages p ON p.run_id = t.run_id AND p.attempt_id = :list_attempt
        JOIN acq_fetch_attempts la ON la.id = p.attempt_id AND la.run_id = p.run_id AND la.source_id = a.source_id
        JOIN acq_download_starts ld ON ld.id = la.download_id AND ld.run_id = p.run_id
        JOIN acq_request_tasks lt ON lt.task_key = ld.task_key AND lt.run_id = p.run_id AND lt.kind = 'list'
        WHERE t.task_key = :task AND t.run_id = :run AND t.kind = 'browser'
          AND t.fingerprint = :f
          AND (t.scope = CAST(:scope AS jsonb) OR
              (:staging AND t.state = 'queued' AND t.scope = CAST(:base_scope AS jsonb)))
          AND a.id = :attempt AND a.run_id = t.run_id AND a.source_id = :s
          AND a.fingerprint = d.fingerprint AND a.fingerprint = t.fingerprint
          AND a.method = 'GET' AND a.url = :request_url AND a.status = 200 AND a.outcome = 'ok'
          AND a.attachment_notice_key IS NULL AND la.status = 200 AND la.outcome = 'ok'
          AND la.fingerprint = ld.fingerprint AND la.fingerprint = lt.fingerprint
          AND p.channel = :channel AND p.window_start = :start AND p.window_end = :end
          AND (p.browser_binding IS NULL OR (
              p.browser_binding->>'task_key' = lt.task_key
              AND p.browser_binding->>'request_fingerprint' = lt.fingerprint
              AND lt.state = 'settled' AND lt.last_attempt = la.id
              AND lt.scope->>0 = p.channel AND lt.scope->>1 = CAST(p.window_start AS text)
              AND lt.scope->>2 = CAST(p.window_end AS text) AND lt.scope->3 = to_jsonb(p.page)
          ))
          AND EXISTS (
              SELECT 1 FROM jsonb_array_elements(p.refs) AS ref
              WHERE ref->>'notice_key' = :k AND ref->>'source_url' = :u
                AND (ref->>'published_on' IS NULL OR ref->>'published_on' = CAST(:p AS text))
          )
        FOR UPDATE OF t
    """), {**params, "staging": staging}).first()
    if row is None or request_origin(row.list_url) != request_origin(params["request_url"]):
        return None
    return row.state


def _same_pair(conn, attempt_id, pair):
    saved = conn.execute(text("SELECT payload FROM acq_browser_receipts WHERE attempt_id = :id"),
                         {"id": attempt_id}).scalar_one()
    fields = ("content_hash", "content_encoding", "media_type", "size_bytes", "projection", "published_on", "rights_tags")
    return all(saved[name] == pair[name] for name in fields)


def _replay(conn, checkpoint, params):
    saved = conn.execute(text("""
        SELECT r.payload, o.version IS NOT NULL AS changed FROM acq_browser_receipts r
        LEFT JOIN acq_source_observations o ON o.browser_attempt_id = r.attempt_id
        WHERE r.attempt_id = :attempt AND r.task_key = :task AND r.source_id = :s AND r.notice_key = :k
    """), params).first()
    if saved is None:
        raise BrowserConflict("settled attempt has no browser receipt")
    fields = {name: value for name, value in saved.payload.items() if name != "binding_digest"}
    if seal_browser_event({**fields, "observation_version": 1}) != checkpoint:
        raise BrowserConflict("browser replay differs from the committed checkpoint")
    return BrowserSubmission(saved.payload["observation_version"], saved.changed, replayed=True)


def _insert_receipt(conn, params, artifact_id, event):
    conn.execute(text("""
        INSERT INTO acq_browser_receipts
            (attempt_id, task_key, list_attempt_id, run_id, source_id, notice_key, artifact_id, payload)
        VALUES (:attempt, :task, :list_attempt, :run, :s, :k, :artifact, CAST(:payload AS jsonb))
    """), {**params, "artifact": artifact_id, "payload": json.dumps(event, ensure_ascii=False)})


def _append_observation(conn, params, artifact_id, version):
    conn.execute(text("""
        INSERT INTO acq_source_observations (source_id, notice_key, version, artifact_id,
            attempt_id, source_url, published_on, observed_at, channel, browser_attempt_id)
        VALUES (:s, :k, :v, :artifact, :attempt, :u, :p, CAST(:t AS timestamptz), :channel, :attempt)
    """), {**params, "v": version, "artifact": artifact_id})
