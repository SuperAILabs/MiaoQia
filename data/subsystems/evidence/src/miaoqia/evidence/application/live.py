"""Turn ordered acquisition observations into evidence snapshots using verified original bytes."""
from __future__ import annotations

from datetime import datetime

from sqlalchemy import Connection, text
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.sql import bindparam

from miaoqia.acquisition.api.browser_pairs import BROWSER_COMPLETED, read_browser_pair, validate_browser_event
from miaoqia.kernel import outbox, payloads, reqctx
from miaoqia.kernel.objectstore import ObjectStore

from ..extensions.packs import DocumentAdapter
from .bindings import advance_live_stream, lock_snapshot_stream
from .grouped import MAX_ORIGINAL, GroupedObservation, check_ref, verified_bytes
from .pipeline import Artifact, Attachment, Pipeline, browser_artifact

LEGACY_COMPLETED = "acquisition.fetch.completed.v1"
COMPLETED = "acquisition.fetch.completed.v2"
GROUPED_COMPLETED = "acquisition.fetch.completed.v3"
GROUPED_PAYLOAD = payloads.validator(GROUPED_COMPLETED, {})
ASSESS_STANDALONE = {kind: payloads.validator(kind, {}) for kind in (LEGACY_COMPLETED, COMPLETED)}


def pending_events(conn: Connection, source_id: str, limit: int):
    return conn.execute(text("""
        SELECT receipt_seq, event_id, envelope FROM evd_pending_live_events
        WHERE source_id = :source ORDER BY receipt_seq LIMIT :limit
    """), {"source": source_id, "limit": limit}).all()


class LiveIngest:
    def __init__(self, store: ObjectStore, pipeline: Pipeline, adapters: dict[str, DocumentAdapter]):
        self.store = store
        self.pipeline = pipeline
        self.adapters = adapters

    def on_completed(self, conn: Connection, env: dict) -> None:
        observed_at, adapter, grouped = self._check_envelope(env)
        source_id = env["data"]["source_id"]
        if adapter is not None and adapter.live_parse is not None:
            self._lock_source(conn, source_id)
            if not adapter.live_parse or conn.execute(text("""
                SELECT 1 FROM evd_pending_live_events WHERE source_id = :source LIMIT 1
            """), {"source": source_id}).first():
                if env["type"] == BROWSER_COMPLETED:
                    data = env["data"]
                    stream = self._stream(conn, data, observed_at, legacy=False, grouped=None, browser=True)
                    if stream is None:
                        return
                    lock_snapshot_stream(conn, source_id, stream, browser_url=data["source_url"])
                    prior = conn.execute(text("""
                        SELECT envelope #>> '{data,binding_digest}' FROM evd_pending_live_events
                        WHERE source_id = :source AND envelope->>'type' = 'acquisition.fetch.completed.v4'
                            AND envelope #>> '{data,notice_key}' = :notice
                            AND envelope #>> '{data,observation_version}' = :version
                    """), {"source": source_id, "notice": data["notice_key"],
                           "version": str(data["observation_version"])}).scalar_one_or_none()
                    if prior is not None:
                        if prior != data["binding_digest"]:
                            raise ValueError("browser observation conflicts with the pending version")
                        return
                    read_browser_pair(self.store, data)
                conn.execute(text("""
                    INSERT INTO evd_pending_live_events (event_id, source_id, envelope)
                    VALUES (:event_id, :source, :envelope) ON CONFLICT (event_id) DO NOTHING
                """).bindparams(bindparam("envelope", type_=JSONB)),
                    {"event_id": env["id"], "source": source_id, "envelope": env})
                return
        self._ingest(conn, env, adapter, observed_at, grouped)

    def drain_one(self, conn: Connection, source_id: str) -> bool:
        """Handle one queued observation in receipt order within the caller's transaction."""
        adapter = self.adapters.get(source_id)
        if adapter is None or adapter.live_parse is not True:
            return False
        self._lock_source(conn, source_id)
        events = pending_events(conn, source_id, 1)
        if not events:
            return False
        (pending,) = events
        if pending.envelope["data"]["source_id"] != source_id:
            raise ValueError("pending observation source does not match its queue")
        observed_at, adapter, grouped = self._check_envelope(pending.envelope)
        self._ingest(conn, pending.envelope, adapter, observed_at, grouped)
        conn.execute(text("DELETE FROM evd_pending_live_events WHERE event_id = :event_id"),
                     {"event_id": pending.event_id})
        return True

    def assess_pending(self, source_id: str, pending) -> dict:
        """Use the queued observation and actual archive, independently of stream state."""
        report = {"receipt_seq": pending.receipt_seq}
        try:
            env = pending.envelope
            outbox.validate(env)
            if (env["id"] != pending.event_id or env["miaoqiazone"] != reqctx.PUBLIC
                    or env["data"].get("source_id") != source_id):
                raise ValueError("pending observation does not match its queue")
            report["event_id"] = env["id"]
            observed_at, adapter, grouped = self._check_envelope(env)
            if validator := ASSESS_STANDALONE.get(env["type"]):
                if not validator.is_valid(env["data"]):
                    raise ValueError("invalid standalone fetch payload")
            data = env["data"]
            report.update(content_hash=data["content_hash"], observation_version=data.get("observation_version"))
            if env["type"] == BROWSER_COMPLETED:
                art = browser_artifact(self.store, data, observed_at)
            else:
                # Only parser input uses this key. Historical identity resolution belongs to replay.
                key = data["source_url"] if env["type"] == LEGACY_COMPLETED else f"notice:{data['notice_key']}"
                art = self._artifact(data, key, observed_at, adapter, grouped)
            result = self.pipeline.assess(art, selected=grouped is not None or env["type"] == BROWSER_COMPLETED)
            return {**report, **result, "status": "assessed"}
        except FileNotFoundError:
            return {**report, "status": "archive_missing"}
        except ValueError:
            return {**report, "status": "input_rejected"}
        except Exception:
            return {**report, "status": "assessment_unavailable"}

    @staticmethod
    def _lock_source(conn: Connection, source_id: str) -> None:
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtextextended(:key, 0))"),
                     {"key": f"evidence/live-source/{source_id}"})

    def _check_envelope(self, env: dict) -> tuple[datetime, DocumentAdapter | None, GroupedObservation | None]:
        data = env["data"]
        kind = env["type"]
        if kind not in (LEGACY_COMPLETED, COMPLETED, GROUPED_COMPLETED, BROWSER_COMPLETED):
            raise ValueError("unsupported fetch completion version")
        if kind == GROUPED_COMPLETED and not GROUPED_PAYLOAD.is_valid(data):
            raise ValueError("invalid grouped fetch payload")
        if kind == BROWSER_COMPLETED:
            validate_browser_event(data)
            if type(data["observation_version"]) is not int:
                raise ValueError("invalid browser observation version")
        if env["source"] != "miaoqia/acquisition" or env["subject"] != data["artifact_ref"]:
            raise ValueError("fetch lineage invalid")
        check_ref(data["artifact_ref"], data["content_hash"])
        adapter = self.adapters.get(data["source_id"])
        grouped = None
        if kind == BROWSER_COMPLETED:
            if (adapter is None or adapter.source_id != data["source_id"]
                    or adapter.browser_projection is not True or type(adapter.live_parse) is not bool
                    or adapter.to_html is not None or adapter.to_selected_html is not None):
                raise ValueError("browser observation requires explicit source and parse admission")
            if adapter.media_type != data["media_type"]:
                raise ValueError("browser original media type does not match the source")
        elif adapter is not None and adapter.browser_projection:
            raise ValueError("source requires a browser observation")
        elif kind == GROUPED_COMPLETED:
            grouped = GroupedObservation.from_event(data)
            grouped.check_adapter(adapter)
        elif adapter is not None and adapter.to_html is None:
            raise ValueError("standalone observation requires a standalone document adapter")
        return datetime.fromisoformat(data["fetched_at"].replace("Z", "+00:00")), adapter, grouped

    def _ingest(self, conn: Connection, env: dict, adapter: DocumentAdapter | None, observed_at: datetime,
                grouped: GroupedObservation | None) -> None:
        d = env["data"]
        browser = env["type"] == BROWSER_COMPLETED
        source_key = self._stream(conn, d, observed_at, legacy=env["type"] == LEGACY_COMPLETED,
                                  grouped=grouped, browser=browser)
        if source_key is None:
            return
        if browser:
            self.pipeline.ingest_browser(conn, d, observed_at, self.store)
            return
        artifact = self._artifact(d, source_key, observed_at, adapter, grouped)
        self.pipeline.ingest(conn, artifact, archived_ref=d["artifact_ref"], grouped=grouped)

    def _artifact(self, d: dict, source_key: str, observed_at: datetime,
                  adapter: DocumentAdapter | None, grouped: GroupedObservation | None) -> Artifact:
        if grouped is not None:
            raw, html = grouped.read(self.store, d["artifact_ref"], adapter)
            # The group endpoint is observation metadata. A normalized notice has no group download link.
            return Artifact(
                source_id=d["source_id"], source_key=source_key, media_type=grouped.media_type, raw=raw,
                rights_tags=tuple(d["rights_tags"]), observed_at=observed_at, html=html,
            )
        raw = verified_bytes(self.store, d["artifact_ref"], d["content_hash"], MAX_ORIGINAL)
        attachments = tuple(Attachment(**part) for part in d.get("attachments", ()))
        for part in attachments:
            if part.status == "available":
                check_ref(part.artifact_ref, part.content_hash)
                content = verified_bytes(self.store, part.artifact_ref, part.content_hash, min(part.size_bytes, MAX_ORIGINAL))
                if len(content) != part.size_bytes:
                    raise ValueError("archived attachment does not match size")
        html, media_type = (adapter.to_html(raw), adapter.media_type) if adapter else (None, "text/html")
        return Artifact(
            source_id=d["source_id"], source_key=source_key, media_type=media_type, raw=raw,
            rights_tags=tuple(d["rights_tags"]), observed_at=observed_at,
            source_url=d["source_url"], html=html, attachments=attachments,
        )

    @staticmethod
    def _stream(conn: Connection, data: dict, observed_at: datetime, *, legacy: bool,
                grouped: GroupedObservation | None, browser: bool) -> str | None:
        source, url = data["source_id"], data["source_url"]
        args = {"s": source, "u": url, "t": observed_at, "h": data["content_hash"]}
        witness = data.get("legacy_artifact")
        addresses = set() if grouped is not None else {url}
        if witness:
            addresses.add(witness["source_url"])
        # Serialize against a historical event for this address while deciding whether to adopt its lineage.
        for address in sorted(addresses):
            conn.execute(text("SELECT pg_advisory_xact_lock(hashtextextended(:key, 0))"),
                         {"key": f"evidence/live-url/{source}|{address}"})
        if legacy:
            if conn.execute(text("SELECT 1 FROM evd_live_urls WHERE source_id = :s AND source_url = :u"), args).first():
                return None
            newer = conn.execute(text("""
                SELECT 1 FROM evd_snapshots WHERE source_id = :s AND source_key = :u AND observed_at > :t
            """), args).first()
            return None if newer else url

        notice, version = data["notice_key"], data["observation_version"]
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtextextended(:key, 0))"),
                     {"key": f"evidence/live-notice/{source}|{notice}"})
        args.update(n=notice, v=version)
        previous = conn.execute(text("""
            SELECT l.version, l.stream_key, g.selector->>'group_key' AS group_key,
                   b.observation_version IS NOT NULL AS browser
            FROM evd_live_observations l
            LEFT JOIN evd_grouped_observations g ON (g.source_id, g.notice_key, g.observation_version)
                                                  = (l.source_id, l.notice_key, l.version)
            LEFT JOIN evd_browser_observations b ON (b.source_id, b.notice_key, b.observation_version)
                                                  = (l.source_id, l.notice_key, l.version)
            WHERE l.source_id = :s AND l.notice_key = :n
        """), args).first()
        notice_stream = f"notice:{notice}"
        if previous is not None and previous.browser != browser:
            raise ValueError("notice key conflicts with the accepted observation type")
        if browser:
            if previous is not None and version <= previous.version:
                binding = conn.execute(text("""
                    SELECT event->>'binding_digest' FROM evd_browser_observations
                    WHERE source_id = :s AND notice_key = :n AND observation_version = :v
                """), args).scalar_one_or_none()
                if binding is not None and binding != data["binding_digest"]:
                    raise ValueError("browser observation conflicts with the accepted version")
                return None
            return notice_stream
        if grouped is not None and previous is not None:
            if previous.stream_key != notice_stream:
                raise ValueError("grouped observation cannot reuse a legacy URL stream")
            if previous.group_key is not None and previous.group_key != grouped.selector.group_key:
                raise ValueError("notice key conflicts with an accepted group")
        if previous and version <= previous.version:
            return None
        if grouped is None and previous is not None and previous.group_key is not None:
            raise ValueError("standalone observation cannot advance a grouped notice stream")

        stream = previous.stream_key if previous else notice_stream
        if previous is None and witness:
            # Acquisition matched the stable part key; evidence independently checks the archived historical bytes.
            # Reusing the existing lineage keeps snapshot, evidence and downstream fact IDs valid.
            proven = conn.execute(text("""
                SELECT 1 FROM evd_snapshots
                WHERE source_id = :s AND source_key = :u AND raw_sha256 = :h AND observed_at <= :t
                    AND NOT EXISTS (
                        SELECT 1 FROM evd_live_observations WHERE source_id = :s AND stream_key = :u
                    )
                LIMIT 1
            """), {**args, "u": witness["source_url"], "h": witness["content_hash"]}).first()
            if proven:
                stream = witness["source_url"]
        for address in sorted({url, stream} & addresses):
            conn.execute(text("""
                INSERT INTO evd_live_urls (source_id, source_url) VALUES (:s, :u) ON CONFLICT DO NOTHING
            """), {**args, "u": address})
        advance_live_stream(conn, data, stream)
        return stream
