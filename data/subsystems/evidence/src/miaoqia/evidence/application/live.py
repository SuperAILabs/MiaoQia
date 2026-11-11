"""Live ingest: acquisition.fetch.completed.v1 → raw artifact from the shared object store → document pipeline.

The source object key is the event's source_url (the announcement's public address), so refetches of the same
announcement become snapshots of one source object. The original is read back by its content-addressed key
and its hash is checked against the event before use. Original bytes are archived unchanged; for non-HTML
originals the pack's document adapter supplies the announcement HTML.
"""
from __future__ import annotations

from datetime import datetime

from sqlalchemy import Connection

from miaoqia.kernel.objectstore import ObjectStore

from ..domain.snapshot import raw_hash
from ..extensions.packs import DocumentAdapter
from .pipeline import Artifact, Pipeline

COMPLETED = "acquisition.fetch.completed.v1"
MAX_ORIGINAL = 20 * 1024 * 1024


class LiveIngest:
    def __init__(self, store: ObjectStore, pipeline: Pipeline, adapters: dict[str, DocumentAdapter]):
        self.store = store
        self.pipeline = pipeline
        self.adapters = adapters

    def on_completed(self, conn: Connection, env: dict) -> None:
        d = env["data"]
        if env["source"] != "miaoqia/acquisition" or env["subject"] != d["artifact_ref"]:
            raise ValueError("fetch lineage invalid")
        if d["artifact_ref"] != "raw/" + d["content_hash"].removeprefix("sha256:"):
            raise ValueError("artifact reference does not match content hash")
        raw = self.store.get(d["artifact_ref"], max_bytes=MAX_ORIGINAL)
        if raw_hash(raw) != d["content_hash"]:
            raise ValueError("archived original does not match content hash")
        adapter = self.adapters.get(d["source_id"])
        html, media_type = (adapter.to_html(raw), adapter.media_type) if adapter else (None, "text/html")
        self.pipeline.ingest(conn, Artifact(
            source_id=d["source_id"], source_key=d["source_url"], media_type=media_type, raw=raw,
            rights_tags=tuple(d["rights_tags"]), observed_at=datetime.fromisoformat(d["fetched_at"].replace("Z", "+00:00")),
            source_url=d["source_url"], html=html))
