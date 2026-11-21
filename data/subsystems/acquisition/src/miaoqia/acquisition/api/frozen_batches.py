"""Frozen test batches pin catalog observations and canonical original references."""
from __future__ import annotations

import re
from dataclasses import asdict, dataclass
from datetime import date
from typing import Literal, Protocol

from miaoqia.kernel.jsoncodec import closed

from . import identifier
from .handlers import GroupSelector

MAX_BATCH_ITEMS = 16
_REF_FIELDS = {"source_id", "notice_key", "observation_version"}
_SELECTOR_FIELDS = {"kind", "version", "group_key", "notice_key"}
_OBJECT_FIELDS = {"artifact_ref", "content_hash", "size_bytes", "media_type"}
_HASH = re.compile(r"sha256:[0-9a-f]{64}")


@dataclass(frozen=True)
class ObservationRef:
    source_id: str
    notice_key: str
    observation_version: int
    part_key: str | None = None

    def __post_init__(self) -> None:
        identifier(self.source_id)
        key = self.notice_key
        if (not isinstance(key, str) or not key.strip() or len(key) > 512 or key.startswith(("http://", "https://"))
                or any(ord(c) < 32 for c in key)):
            raise ValueError("invalid notice key")
        key.encode("utf-8")
        if type(self.observation_version) is not int or not 1 <= self.observation_version < 2**63:
            raise ValueError("invalid observation version")
        part = self.part_key
        if part is not None and (not isinstance(part, str) or not part.strip() or len(part) > 512
                                 or any(ord(c) < 32 for c in part)):
            raise ValueError("invalid attachment key")

    @classmethod
    def parse(cls, value: object) -> ObservationRef:
        fields = _REF_FIELDS | ({"part_key"} if isinstance(value, dict) and "part_key" in value else set())
        return cls(**closed(value, fields))


def validate_selection(refs: tuple[ObservationRef, ...]) -> None:
    if (not isinstance(refs, tuple) or not 1 <= len(refs) <= MAX_BATCH_ITEMS
            or any(not isinstance(ref, ObservationRef) for ref in refs)
            or len(set(refs)) != len(refs)):
        raise ValueError("select up to 16 distinct observations")


def _batch_items(value: object, version: int) -> list:
    batch = closed(value, {"schema_version", "items"})
    items = batch["items"]
    if (type(batch["schema_version"]) is not int or batch["schema_version"] != version
            or not isinstance(items, list) or not 1 <= len(items) <= MAX_BATCH_ITEMS):
        raise ValueError("invalid frozen batch")
    return items


@dataclass(frozen=True)
class FrozenOriginal:
    ref: ObservationRef
    content_hash: str

    def __post_init__(self) -> None:
        if (not isinstance(self.ref, ObservationRef) or not isinstance(self.content_hash, str)
                or not _HASH.fullmatch(self.content_hash)):
            raise ValueError("invalid frozen original")

    @classmethod
    def parse(cls, value: object) -> FrozenOriginal:
        fields = _REF_FIELDS | ({"part_key"} if isinstance(value, dict) and "part_key" in value else set())
        item = closed(value, fields | {"content_hash", "artifact_ref"})
        original = cls(ObservationRef.parse({name: item[name] for name in fields}), item["content_hash"])
        if item["artifact_ref"] != original.artifact_ref:
            raise ValueError("invalid original reference")
        return original

    @property
    def artifact_ref(self) -> str:
        return f"raw/{self.content_hash.removeprefix('sha256:')}"

    def dto(self) -> dict:
        fields = {"source_id": self.ref.source_id, "notice_key": self.ref.notice_key,
                  "observation_version": self.ref.observation_version, "content_hash": self.content_hash,
                  "artifact_ref": self.artifact_ref}
        if self.ref.part_key is not None:
            fields["part_key"] = self.ref.part_key
        return fields


@dataclass(frozen=True)
class FrozenBatch:
    items: tuple[FrozenOriginal, ...]

    def __post_init__(self) -> None:
        if not isinstance(self.items, tuple) or any(not isinstance(item, FrozenOriginal) for item in self.items):
            raise ValueError("invalid batch items")
        validate_selection(tuple(item.ref for item in self.items))

    @classmethod
    def parse(cls, value: object) -> FrozenBatch:
        return cls(tuple(FrozenOriginal.parse(item) for item in _batch_items(value, 1)))

    def dto(self) -> dict:
        return {"schema_version": 1, "items": [item.dto() for item in self.items]}


@dataclass(frozen=True)
class FrozenRecord:
    """A pinned member and the checksum of its complete publisher response."""

    original: FrozenOriginal
    selector: GroupSelector
    record_digest: str
    published_on: date

    def __post_init__(self) -> None:
        if (not isinstance(self.original, FrozenOriginal) or self.ref.part_key is not None
                or not isinstance(self.selector, GroupSelector) or self.selector.notice_key != self.ref.notice_key
                or not isinstance(self.record_digest, str) or not _HASH.fullmatch(self.record_digest)
                or type(self.published_on) is not date):
            raise ValueError("invalid frozen record")
        self.selector.group_key.encode("utf-8")

    @property
    def ref(self) -> ObservationRef:
        return self.original.ref

    @classmethod
    def parse(cls, value: object) -> FrozenRecord:
        record = closed(value, _REF_FIELDS | {"content_hash", "selector", "record_digest", "published_on"})
        ref = ObservationRef.parse({name: record[name] for name in _REF_FIELDS})
        selector = GroupSelector(**closed(record["selector"], _SELECTOR_FIELDS))
        published = record["published_on"]
        if not isinstance(published, str) or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", published):
            raise ValueError("invalid publication date")
        return cls(FrozenOriginal(ref, record["content_hash"]), selector, record["record_digest"],
                   date.fromisoformat(published))

    def dto(self) -> dict:
        return {"source_id": self.ref.source_id, "notice_key": self.ref.notice_key,
                "observation_version": self.ref.observation_version, "content_hash": self.original.content_hash,
                "selector": asdict(self.selector), "record_digest": self.record_digest,
                "published_on": self.published_on.isoformat()}


@dataclass(frozen=True)
class FrozenRecords:
    items: tuple[FrozenRecord, ...]

    def __post_init__(self) -> None:
        if not isinstance(self.items, tuple) or any(not isinstance(item, FrozenRecord) for item in self.items):
            raise ValueError("invalid frozen record items")
        validate_selection(tuple(item.ref for item in self.items))

    @classmethod
    def parse(cls, value: object) -> FrozenRecords:
        return cls(tuple(FrozenRecord.parse(item) for item in _batch_items(value, 2)))

    def dto(self) -> dict:
        return {"schema_version": 2, "items": [item.dto() for item in self.items]}


@dataclass(frozen=True)
class SelectedRecord:
    ref: ObservationRef
    published_on: date
    html: str

    def dto(self) -> dict:
        return {"schema_version": 1, "kind": "selected_notice", "media_type": "text/html",
                "source_id": self.ref.source_id, "notice_key": self.ref.notice_key,
                "observation_version": self.ref.observation_version,
                "published_on": self.published_on.isoformat(), "html": self.html}


@dataclass(frozen=True)
class FrozenBrowserObject:
    artifact_ref: str
    content_hash: str
    size_bytes: int
    media_type: str

    def __post_init__(self) -> None:
        if (not isinstance(self.artifact_ref, str)
                or not isinstance(self.content_hash, str) or not _HASH.fullmatch(self.content_hash)
                or type(self.size_bytes) is not int or not 1 <= self.size_bytes <= 20 * 1024 * 1024
                or not isinstance(self.media_type, str) or not 1 <= len(self.media_type) <= 128):
            raise ValueError("invalid browser object descriptor")
        self.media_type.encode("utf-8")


@dataclass(frozen=True)
class FrozenBrowserObservation:
    ref: ObservationRef
    original: FrozenBrowserObject
    projection: FrozenBrowserObject
    projection_version: int
    binding_digest: str

    def __post_init__(self) -> None:
        if (not isinstance(self.ref, ObservationRef) or self.ref.part_key is not None
                or not isinstance(self.original, FrozenBrowserObject)
                or not isinstance(self.projection, FrozenBrowserObject)
                or self.original.artifact_ref != f"raw/{self.original.content_hash[7:]}"
                or self.projection.artifact_ref != f"projection/{self.projection.content_hash[7:]}"
                or self.projection.media_type != "application/json" or self.projection.size_bytes > 1024 * 1024
                or type(self.projection_version) is not int or self.projection_version != 1
                or not isinstance(self.binding_digest, str) or not _HASH.fullmatch(self.binding_digest)):
            raise ValueError("invalid frozen browser observation")

    @classmethod
    def parse(cls, value: object) -> FrozenBrowserObservation:
        item = closed(value, _REF_FIELDS | {"original", "projection", "binding_digest"})
        ref = ObservationRef.parse({key: item[key] for key in _REF_FIELDS})
        original = FrozenBrowserObject(**closed(item["original"], _OBJECT_FIELDS))
        projection = closed(item["projection"], _OBJECT_FIELDS | {"version"})
        return cls(ref, original, FrozenBrowserObject(**{key: projection[key] for key in _OBJECT_FIELDS}),
                   projection["version"], item["binding_digest"])

    @classmethod
    def from_receipt(cls, event: dict) -> FrozenBrowserObservation:
        """Select the manifest fields from a validated browser completion receipt."""
        return cls.parse({
            **{key: event[key] for key in _REF_FIELDS | {"projection", "binding_digest"}},
            "original": {key: event[key] for key in _OBJECT_FIELDS},
        })

    def dto(self) -> dict:
        return {"source_id": self.ref.source_id, "notice_key": self.ref.notice_key,
                "observation_version": self.ref.observation_version, "original": asdict(self.original),
                "projection": {**asdict(self.projection), "version": self.projection_version},
                "binding_digest": self.binding_digest}


@dataclass(frozen=True)
class FrozenBrowserBatch:
    items: tuple[FrozenBrowserObservation, ...]

    def __post_init__(self) -> None:
        if not isinstance(self.items, tuple) or any(not isinstance(item, FrozenBrowserObservation) for item in self.items):
            raise ValueError("invalid frozen browser items")
        validate_selection(tuple(item.ref for item in self.items))

    @classmethod
    def parse(cls, value: object) -> FrozenBrowserBatch:
        return cls(tuple(FrozenBrowserObservation.parse(item) for item in _batch_items(value, 3)))

    def dto(self) -> dict:
        return {"schema_version": 3, "items": [item.dto() for item in self.items]}


@dataclass(frozen=True)
class SelectedProjection:
    ref: ObservationRef
    published_on: date
    document: dict

    def dto(self) -> dict:
        return {"schema_version": 1, "kind": "browser_projection", "media_type": "application/json",
                "source_id": self.ref.source_id, "notice_key": self.ref.notice_key,
                "observation_version": self.ref.observation_version,
                "published_on": self.published_on.isoformat(), "document": self.document}


Operation = Literal["freeze", "read", "restore"]
RecordOperation = Literal["freeze-records", "read-record", "restore-record-original"]
BrowserOperation = Literal["freeze-browser", "read-projection", "verify-browser", "read-browser-original",
                           "restore-browser-original", "restore-browser-projection"]


class AuthorizeOriginal(Protocol):
    def __call__(self, operation: Operation, original: FrozenOriginal, /) -> bool:
        """Decide with current rights and a trusted caller context. Called again after verification."""
        ...


class AuthorizeRecord(Protocol):
    def __call__(self, operation: RecordOperation, record: FrozenRecord, /) -> bool:
        """Check projection rights or, for restore-record-original, whole-original restoration rights."""
        ...


class AuthorizeBrowser(Protocol):
    def __call__(self, operation: BrowserOperation, item: FrozenBrowserObservation, /) -> bool:
        """Decide current rights in the trusted caller context for this exact operation and observation."""
        ...


class SelectRecord(Protocol):
    def __call__(self, raw: bytes, selector: GroupSelector, *, published_on: date, record_digest: str) -> str:
        """Revalidate the member and return its HTML projection."""
        ...


class FrozenBatchDenied(PermissionError):
    """Current permission or an authoritative observation binding could not be established."""


class FrozenObjectError(RuntimeError):
    def __init__(self, code: str):
        super().__init__(code)
        self.code = code
