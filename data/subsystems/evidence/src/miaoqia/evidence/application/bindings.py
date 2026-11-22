"""Coordinate snapshot streams and persist or read their immutable input anchors."""
from datetime import datetime

from sqlalchemy import text
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.sql import bindparam

from miaoqia.acquisition.api.browser_pairs import validate_browser_event

_SNAPSHOTS_WITH_BINDINGS = """
    SELECT s.*, to_jsonb(g) AS grouped_binding, b.event AS browser_binding
    FROM evd_snapshots s
    LEFT JOIN evd_grouped_observations g ON g.snapshot_id = s.id AND g.is_anchor
    LEFT JOIN evd_browser_observations b ON b.snapshot_id = s.id AND b.is_anchor
"""


def advance_live_stream(conn, data: dict, stream: str) -> None:
    conn.execute(text("""
        INSERT INTO evd_live_observations (source_id, notice_key, version, stream_key) VALUES (:s, :n, :v, :stream)
        ON CONFLICT (source_id, notice_key) DO UPDATE SET version = EXCLUDED.version
    """), {"s": data["source_id"], "n": data["notice_key"], "v": data["observation_version"], "stream": stream})


def snapshot_with_binding(conn, snapshot_ref: str):
    return conn.execute(text(_SNAPSHOTS_WITH_BINDINGS + "WHERE s.id = :i"),
                        {"i": snapshot_ref}).mappings().one_or_none()


def lock_snapshot_stream(conn, source_id: str, source_key: str, *, browser_url: str | None):
    # Browser observations use a stable notice key. Also lock the URL used by direct and legacy callers.
    keys = {source_key} if browser_url is None else {source_key, browser_url}
    for key in sorted(keys):
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": f"{source_id}|{key}"})
    if browser_url is not None:
        conflict = conn.execute(text("""
            SELECT 1 FROM evd_snapshots WHERE source_id = :s AND source_key = :url LIMIT 1
        """), {"s": source_id, "url": browser_url}).first()
    else:
        conflict = conn.execute(text("""
            SELECT 1 FROM evd_browser_observations
            WHERE source_id = :s AND event->>'source_url' = :url LIMIT 1
        """), {"s": source_id, "url": source_key}).first()
    if conflict is not None:
        raise ValueError("browser observation conflicts with a URL-keyed snapshot")
    latest = conn.execute(text(_SNAPSHOTS_WITH_BINDINGS + """
        WHERE s.source_id = :s AND s.source_key = :k ORDER BY s.seq DESC LIMIT 1
    """), {"s": source_id, "k": source_key}).first()
    if latest is not None and (latest.browser_binding is None) != (browser_url is None):
        raise ValueError("snapshot stream conflicts with the browser observation type")
    return latest


def bind_browser_observation(conn, event: dict, snapshot_ref: str, *, is_anchor: bool) -> None:
    advance_live_stream(conn, event, f"notice:{event['notice_key']}")
    conn.execute(text("""
        INSERT INTO evd_browser_observations
            (source_id, notice_key, observation_version, snapshot_id, event, is_anchor)
        VALUES (:source, :notice, :version, :snapshot, :event, :anchor)
    """).bindparams(bindparam("event", type_=JSONB)),
        {"source": event["source_id"], "notice": event["notice_key"], "version": event["observation_version"],
         "snapshot": snapshot_ref, "event": event, "anchor": is_anchor})


def browser_anchor(snapshot) -> dict:
    event = validate_browser_event(snapshot["browser_binding"])
    if (snapshot["grouped_binding"] is not None
            or snapshot["source_id"] != event["source_id"]
            or snapshot["source_key"] != f"notice:{event['notice_key']}"
            or snapshot["artifact_ref"] != event["artifact_ref"]
            or snapshot["raw_sha256"] != event["content_hash"]
            or snapshot["media_type"] != event["media_type"]
            or snapshot["rights_tags"] != event["rights_tags"]
            or snapshot["observed_at"] != datetime.fromisoformat(event["fetched_at"].replace("Z", "+00:00"))):
        raise ValueError("browser anchor does not match the snapshot")
    return event
