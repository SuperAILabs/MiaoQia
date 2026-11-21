"""Validate one publisher response and atomically register its grouped observations."""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass
from datetime import datetime
from functools import cache

from sqlalchemy import Connection, text

from miaoqia.kernel import outbox, payloads

from ..api.handlers import FetchResponse, GroupedNotice, NoticeGroup, SourceHandler, Window, expand_notice_group
from ..infrastructure.transport import MAX_BODY, TransportError, request_origin
from . import discovery, fetch
from .collection_leases import CollectionLease

COMPLETED = "acquisition.fetch.completed.v3"


class GroupConflict(ValueError):
    """The response cannot be reconciled with its task or accepted notice history."""


@dataclass(frozen=True)
class ValidatedGroup:
    group: NoticeGroup
    channel: str
    content_hash: str
    media_type: str
    size_bytes: int
    members: tuple[GroupedNotice, ...]

    @property
    def artifact_ref(self) -> str:
        return f"raw/{self.content_hash.removeprefix('sha256:')}"


@dataclass(frozen=True)
class GroupSubmission:
    artifact_id: int
    archived: int
    unchanged: int
    stale: int
    outside_window: int
    replayed: bool


def validate_group(handler: SourceHandler, channel: str, group: NoticeGroup, response: FetchResponse) -> ValidatedGroup:
    """Validate the complete, pinned response before the caller stores its original bytes."""
    if (not channel or response.status != 200 or response.retry_after or response.redirect_to
            or not 1 <= len(response.media_type) <= 128 or not 1 <= len(response.body) <= MAX_BODY
            or not discovery.matches_list_target(group.request, response)):
        raise ValueError("invalid group response")
    members = expand_notice_group(handler, channel, group, response)
    origin = request_origin(group.request.url)
    try:
        same_origin = all(request_origin(member.ref.source_url) == origin for member in members)
    except TransportError:
        same_origin = False
    if not same_origin:
        raise ValueError("notice locator outside the group origin")
    return ValidatedGroup(group, channel, f"sha256:{hashlib.sha256(response.body).hexdigest()}",
                          response.media_type, len(response.body),
                          tuple(sorted(members, key=lambda member: member.selector.notice_key)))


def submit_group(conn: Connection, *, lease: CollectionLease, window: Window, list_attempt_id: int,
                 task_key: str, attempt_id: int, received_at: datetime, capture: ValidatedGroup) -> GroupSubmission:
    """Settle a staged attempt after verifying its object is durable at capture.artifact_ref.

    The caller owns the outer transaction and the capture checkpoint. The committed discovery page
    must include {group_key, request_fingerprint} in refs. The group task scope pins the page and
    the accepted attempt's hash, media type, size and receipt time.
    A current lease may replay a capture made by an earlier generation of the same run.
    """
    if not conn.in_transaction():
        raise RuntimeError("group submission requires an outer transaction")
    if (not lease.window.since <= window.since <= window.until <= lease.window.until
            or received_at.utcoffset() is None):
        raise GroupConflict("group receipt outside its collection window or missing receipt timezone")
    request = capture.group.request
    fingerprint = request.fingerprint()
    params = {"s": lease.source_id, "run": lease.run_id, "channel": capture.channel, "start": window.since,
              "end": window.until, "since": window.since.isoformat(), "until": window.until.isoformat(),
              "list_attempt": list_attempt_id, "task": task_key, "attempt": attempt_id,
              "group": capture.group.group_key, "f": fingerprint, "method": request.method,
              "u": fetch._attempt_url(request.url), "t": received_at, "instant": fetch._instant(received_at),
              "h": capture.content_hash,
              "a": capture.artifact_ref, "m": capture.media_type, "n": capture.size_bytes,
              "group_ref": json.dumps([{"group_key": capture.group.group_key,
                                        "request_fingerprint": fingerprint}])}
    members = [{"selector": asdict(member.selector), "record_digest": member.record_digest,
                "source_url": member.ref.source_url, "published_on": member.ref.published.isoformat()}
               for member in capture.members]

    with conn.begin_nested():
        lease.check(conn)
        allowed = conn.execute(text("""
            SELECT public.acq_collection_allowed(source_id) FROM acq_sources
            WHERE source_id = :s AND state = 'active'
        """), params).scalar_one_or_none()
        if not allowed:
            raise GroupConflict("source cannot archive a group")
        state = _bound_task(conn, params)
        if state == "settled":
            return _replay(conn, params, members)
        if state != "staged":
            raise GroupConflict("group attempt is not staged against the recorded listing")

        accepted = _plan_members(conn, params, window, capture.members, members)
        artifact_id = fetch.Fetcher._artifact(conn, {**params, "part": f"group:{capture.group.group_key}"})
        conn.execute(text("""
            INSERT INTO acq_group_receipts (attempt_id, task_key, list_attempt_id, run_id, source_id, group_key,
                artifact_id, channel, window_start, window_end, received_at, media_type, size_bytes, members)
            VALUES (:attempt, :task, :list_attempt, :run, :s, :group,
                :artifact, :channel, :start, :end, :t, :m, :n, CAST(:members AS jsonb))
        """), {**params, "artifact": artifact_id, "members": json.dumps(members, ensure_ascii=False)})

        for member, check, event in accepted:
            if event is not None:
                _append_observation(conn, params, artifact_id, member, check)
                fetch.writer.append(conn, event)
            fetch.Fetcher._advance_check(conn, check)
        fetch.Fetcher._task_state(conn, task_key, "settled", attempt_id)
        return _result(artifact_id, members, replayed=False)


def _bound_task(conn, params):
    bound = conn.execute(text("""
        SELECT t.state, la.url AS list_url FROM acq_request_tasks t
        JOIN acq_download_starts d ON d.task_key = t.task_key AND d.run_id = t.run_id
        JOIN acq_fetch_attempts a ON a.download_id = d.id AND a.id = t.last_attempt
        JOIN acq_discovery_pages p ON p.run_id = t.run_id AND p.attempt_id = :list_attempt
        JOIN acq_fetch_attempts la ON la.id = p.attempt_id AND la.run_id = p.run_id AND la.source_id = a.source_id
        JOIN acq_download_starts ld ON ld.id = la.download_id AND ld.run_id = p.run_id
        JOIN acq_request_tasks lt ON lt.task_key = ld.task_key AND lt.run_id = p.run_id AND lt.kind = 'list'
        WHERE t.task_key = :task AND t.run_id = :run AND t.kind = 'group' AND t.fingerprint = :f
          AND a.id = :attempt AND a.run_id = t.run_id AND a.source_id = :s
          AND a.fingerprint = d.fingerprint AND a.fingerprint = t.fingerprint
          AND a.method = :method AND a.url = :u AND a.status = 200 AND a.outcome = 'ok'
          AND a.attachment_notice_key IS NULL AND la.status = 200 AND la.outcome = 'ok'
          AND la.fingerprint = ld.fingerprint
          AND p.channel = :channel AND p.window_start = :start AND p.window_end = :end
          AND p.refs @> CAST(:group_ref AS jsonb)
          AND t.scope->>0 = p.channel AND t.scope->>1 = :since
          AND t.scope->>2 = :until AND t.scope->>3 = :group
          AND t.scope->4 = to_jsonb(p.attempt_id) AND t.scope->5 = to_jsonb(p.page)
          AND t.scope->6 @> jsonb_build_object('attempt_id', a.id, 'content_hash', CAST(:h AS text),
              'media_type', CAST(:m AS text), 'size_bytes', CAST(:n AS integer), 'received_at', CAST(:instant AS text))
        FOR UPDATE OF t
    """), params).first()
    if bound is None:
        return None
    try:
        if request_origin(bound.list_url) != request_origin(params["u"]):
            return None
    except TransportError:
        return None
    return bound.state


def _replay(conn, params, members):
    saved = conn.execute(text("""
        SELECT r.artifact_id, r.members FROM acq_group_receipts r
        JOIN acq_raw_artifacts a ON a.id = r.artifact_id
        WHERE r.attempt_id = :attempt AND r.task_key = :task AND r.list_attempt_id = :list_attempt
          AND r.group_key = :group AND r.received_at = :t AND r.media_type = :m AND r.size_bytes = :n
          AND a.content_hash = :h AND a.artifact_ref = :a
    """), params).first()
    if saved is None or [{k: v for k, v in item.items() if k not in ("outcome", "version")}
                         for item in saved.members] != members:
        raise GroupConflict("group replay disagrees with its original receipt")
    return _result(saved.artifact_id, saved.members, replayed=True)


def _plan_members(conn, params, window, notices, members):
    keys = {"s": params["s"], "group": params["group"],
            "keys": [notice.selector.notice_key for notice in notices]}
    conflict = conn.execute(text("""
        SELECT browser_attempt_id FROM acq_source_observations
        WHERE source_id = :s AND notice_key = ANY(:keys) AND (group_key <> :group OR browser_attempt_id IS NOT NULL)
        LIMIT 1
    """), keys).first()
    if conflict:
        if conflict.browser_attempt_id is not None:
            raise GroupConflict("notice is already bound to a browser capture")
        raise GroupConflict("notice is already bound to a different group")

    latest = {row.notice_key: row for row in conn.execute(text("""
        SELECT c.notice_key, c.version, c.attempt_id, c.legacy_artifact_id, a.run_id,
               o.selector, o.record_digest, o.published_on
        FROM acq_detail_checks c JOIN acq_source_observations o USING (source_id, notice_key, version)
        JOIN acq_fetch_attempts a ON a.id = c.attempt_id
        WHERE c.source_id = :s AND c.notice_key = ANY(:keys)
    """), keys)}
    accepted = []
    for notice, member in zip(notices, members, strict=True):
        key = notice.selector.notice_key
        if not window.since <= notice.ref.published <= window.until:
            member["outcome"] = "outside_window"
            continue
        last = latest.get(key)
        if last and last.selector is None and last.legacy_artifact_id is not None:
            raise GroupConflict("notice is already linked to a legacy original")
        same = False
        if last and last.selector is not None:
            same = (last.record_digest, last.selector["kind"], last.selector["version"]) == (
                notice.record_digest, notice.selector.kind, notice.selector.version)
            if same and last.published_on != notice.ref.published:
                raise GroupConflict("member date changed without a new digest")
        if last and (last.run_id or 0, last.attempt_id) >= (params["run"], params["attempt"]):
            member["outcome"] = "stale"
            continue
        version = (last.version if last else 0) + int(not same)
        member.update(outcome="unchanged" if same else "archived", version=version)
        check = {"s": params["s"], "k": key, "v": version, "attempt": params["attempt"],
                 "t": params["t"], "legacy": last.legacy_artifact_id if last else None}
        event = None if same else _observation_event(params, member, check)
        accepted.append((member, check, event))
    return accepted


def _append_observation(conn, params, artifact_id, member, check):
    conn.execute(text("""
        INSERT INTO acq_source_observations (source_id, notice_key, version, artifact_id, attempt_id,
            source_url, published_on, observed_at, channel, selector, record_digest)
        VALUES (:s, :k, :v, :artifact, :attempt, :u, CAST(:published_on AS date), :t, :channel,
            CAST(:selector AS jsonb), :record_digest)
    """), {**check, "artifact": artifact_id, "channel": params["channel"], "u": member["source_url"],
           "published_on": member["published_on"], "selector": json.dumps(member["selector"], ensure_ascii=False),
           "record_digest": member["record_digest"]})


def _observation_event(params, member, check):
    data = {"source_id": check["s"], "notice_key": check["k"], "observation_version": check["v"],
            "artifact_ref": params["a"], "rights_tags": fetch.rights_tags(check["s"]), "content_hash": params["h"],
            "fetched_at": fetch._instant(check["t"]), "source_url": member["source_url"],
            "selector": member["selector"], "record_digest": member["record_digest"],
            "published_on": member["published_on"], "media_type": params["m"], "size_bytes": params["n"]}
    _payload_validator().validate(data)
    return outbox.Event(type=COMPLETED, subject=params["a"], partition_key=f"{check['s']}|{check['k']}", data=data)


@cache
def _payload_validator():
    return payloads.validator(COMPLETED, {})


def _result(artifact_id, members, *, replayed):
    outcomes = [member["outcome"] for member in members]
    return GroupSubmission(artifact_id, outcomes.count("archived"), outcomes.count("unchanged"),
                           outcomes.count("stale"), outcomes.count("outside_window"), replayed)
