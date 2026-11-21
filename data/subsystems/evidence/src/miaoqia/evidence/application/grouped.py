"""Verify and persist the selected member of an unmodified group response."""
from __future__ import annotations

from dataclasses import asdict, dataclass
from datetime import date

from sqlalchemy import text
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.sql import bindparam

from miaoqia.acquisition.api.handlers import GroupSelector
from miaoqia.kernel.objectstore import ObjectStore

from ..domain.snapshot import raw_hash
from ..extensions.packs import DocumentAdapter

MAX_ORIGINAL = 20 * 1024 * 1024


def check_ref(ref: str, digest: str) -> None:
    if ref != "raw/" + digest.removeprefix("sha256:"):
        raise ValueError("artifact reference does not match content hash")


def verified_bytes(store: ObjectStore, ref: str, digest: str, limit: int) -> bytes:
    raw = store.get(ref, max_bytes=limit)
    if raw_hash(raw) != digest:
        raise ValueError("archived original does not match content hash")
    return raw


@dataclass(frozen=True)
class GroupedObservation:
    source_id: str
    notice_key: str
    observation_version: int
    selector: GroupSelector
    published_on: date
    record_digest: str
    raw_sha256: str
    media_type: str
    size_bytes: int

    @classmethod
    def from_event(cls, data: dict) -> GroupedObservation:
        version, size = data["observation_version"], data["size_bytes"]
        if type(version) is not int or not 1 <= version <= 2**63 - 1 or type(size) is not int:
            raise ValueError("invalid grouped observation version or size")
        return cls(data["source_id"], data["notice_key"], version,
                   GroupSelector(**data["selector"]), date.fromisoformat(data["published_on"]),
                   data["record_digest"], data["content_hash"], data["media_type"], size)

    @classmethod
    def from_row(cls, row) -> GroupedObservation:
        return cls(row["source_id"], row["notice_key"], row["observation_version"],
                   GroupSelector(**row["selector"]), date.fromisoformat(row["published_on"]),
                   row["record_digest"], row["raw_sha256"], row["media_type"], row["size_bytes"])

    @classmethod
    def from_snapshot(cls, snapshot) -> GroupedObservation:
        observation = cls.from_row(snapshot["grouped_binding"])
        if (snapshot["source_id"] != observation.source_id
                or snapshot["source_key"] != f"notice:{observation.notice_key}"
                or snapshot["raw_sha256"] != observation.raw_sha256
                or snapshot["media_type"] != observation.media_type):
            raise ValueError("grouped anchor does not match the snapshot")
        return observation

    def check_adapter(self, adapter: DocumentAdapter | None) -> None:
        if (adapter is None or adapter.source_id != self.source_id or not callable(adapter.to_selected_html)
                or type(adapter.live_parse) is not bool):
            raise ValueError("grouped observation requires a selection adapter with explicit parse admission")
        if self.media_type != adapter.media_type:
            raise ValueError("grouped original media type does not match the selection adapter")
        if self.notice_key != self.selector.notice_key:
            raise ValueError("grouped selector notice key does not match the observation")

    def read(self, store: ObjectStore, ref: str, adapter: DocumentAdapter | None) -> tuple[bytes, str]:
        self.check_adapter(adapter)
        if not 1 <= self.size_bytes <= MAX_ORIGINAL:
            raise ValueError("invalid grouped original size")
        check_ref(ref, self.raw_sha256)
        raw = verified_bytes(store, ref, self.raw_sha256, self.size_bytes)
        if len(raw) != self.size_bytes:
            raise ValueError("archived grouped original does not match size")
        html = adapter.to_selected_html(raw, self.selector, published_on=self.published_on,
                                        record_digest=self.record_digest)
        if not isinstance(html, str) or not html.strip():
            raise ValueError("selection adapter did not produce a document")
        return raw, html

    def same_member(self, anchored) -> bool:
        return (anchored["selector"] == asdict(self.selector)
                and anchored["record_digest"] == self.record_digest
                and anchored["published_on"] == self.published_on.isoformat())

    def bind(self, conn, snapshot_id: str, *, is_anchor: bool) -> None:
        conn.execute(text("""
            INSERT INTO evd_grouped_observations
                (source_id, notice_key, observation_version, snapshot_id, selector, published_on,
                 record_digest, raw_sha256, media_type, size_bytes, is_anchor)
            VALUES (:source_id, :notice_key, :observation_version, :snapshot_id, :selector, :published_on,
                    :record_digest, :raw_sha256, :media_type, :size_bytes, :is_anchor)
        """).bindparams(bindparam("selector", type_=JSONB)),
            {**asdict(self), "snapshot_id": snapshot_id, "is_anchor": is_anchor})
