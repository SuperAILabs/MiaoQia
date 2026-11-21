"""Resolve frozen notice observations against acquisition's append-only catalog."""
from __future__ import annotations

from collections.abc import Callable, Mapping
from dataclasses import dataclass
from datetime import date
from functools import partial
from hashlib import sha256
from typing import Literal

from sqlalchemy import text

from miaoqia.kernel import reqctx
from miaoqia.kernel.db import Database
from miaoqia.kernel.objectstore import ObjectConflict, ObjectStore

from ..api import instant
from ..api.browser_pairs import read_browser_pair, validate_browser_event
from ..api.frozen_batches import (
    AuthorizeBrowser, AuthorizeOriginal, AuthorizeRecord, BrowserOperation, FrozenBatch, FrozenBatchDenied,
    FrozenBrowserBatch, FrozenBrowserObject, FrozenBrowserObservation, FrozenObjectError, FrozenOriginal,
    FrozenRecord, FrozenRecords, ObservationRef, Operation, RecordOperation, SelectedProjection, SelectedRecord,
    SelectRecord, validate_selection,
)
from ..api.handlers import GroupSelector
from ..infrastructure.transport import MAX_BODY

_OBSERVATION = text("""
    SELECT a.content_hash, a.artifact_ref, a.size_bytes, o.selector, o.record_digest, o.published_on
    FROM acq_source_observations o
    LEFT JOIN acq_observation_attachments p ON
        p.source_id = o.source_id AND p.notice_key = o.notice_key AND p.version = o.version
        AND p.part_key = :part_key AND p.status = 'available'
    JOIN acq_raw_artifacts a ON a.source_id = o.source_id
        AND a.id = CASE WHEN CAST(:part_key AS text) IS NULL THEN o.artifact_id ELSE p.artifact_id END
    WHERE o.source_id = :source_id AND o.notice_key = :notice_key AND o.version = :version
      AND (o.selector IS NOT NULL) = :grouped
      AND o.browser_attempt_id IS NULL
""")
_BROWSER_OBSERVATION = text("""
    SELECT a.content_hash, a.artifact_ref, a.size_bytes, o.source_url, o.published_on, o.observed_at,
        r.attempt_id, r.run_id, r.list_attempt_id, r.task_key, r.payload
    FROM acq_source_observations o
    JOIN acq_browser_receipts r ON r.attempt_id = o.browser_attempt_id AND r.attempt_id = o.attempt_id
        AND r.source_id = o.source_id AND r.notice_key = o.notice_key AND r.artifact_id = o.artifact_id
    JOIN acq_raw_artifacts a ON a.source_id = o.source_id AND a.id = o.artifact_id
        AND a.source_key = 'browser:' || o.notice_key
    WHERE o.source_id = :source_id AND o.notice_key = :notice_key AND o.version = :version
        AND o.selector IS NULL
""")


@dataclass(frozen=True)
class _ArchivedOriginal:
    original: FrozenOriginal
    size_bytes: int
    record: FrozenRecord | None = None

    @property
    def artifact_ref(self) -> str:
        return self.original.artifact_ref

    @property
    def content_hash(self) -> str:
        return self.original.content_hash


class FrozenBatches:
    """Trusted composition supplies the current authorizer. A manifest is only a set of pinned references."""

    def __init__(self, database: Database, store: ObjectStore, *, authorize: AuthorizeOriginal | None = None,
                 authorize_record: AuthorizeRecord | None = None,
                 authorize_browser: AuthorizeBrowser | None = None,
                 record_adapters: Mapping[str, SelectRecord] | None = None):
        if not any(map(callable, (authorize, authorize_record, authorize_browser))):
            raise ValueError("current authorization callback required")
        self._db = database
        self._store = store
        self._authorize = authorize
        self._authorize_record = authorize_record
        self._authorize_browser = authorize_browser
        self._record_adapters = dict(record_adapters or {})

    def freeze(self, refs: tuple[ObservationRef, ...]) -> FrozenBatch:
        """Freeze a curated selection of explicit versions with verifiable originals."""
        validate_selection(refs)
        originals = [self._lookup(ref) for ref in refs]
        for archive in originals:
            self._read_checked("freeze", archive, self._store, "original_missing")
        # Earlier entries may have been revoked while subsequent originals were being verified.
        for archive in originals[:-1]:
            self._require("freeze", archive.original)
        return FrozenBatch(tuple(archive.original for archive in originals))

    def read(self, batch: FrozenBatch, index: int) -> bytes:
        archive = self._resolve(batch, index)
        return self._read_checked("read", archive, self._store, "original_missing")

    def freeze_records(self, refs: tuple[ObservationRef, ...]) -> FrozenRecords:
        """Pin explicitly selected grouped observations after verifying each projection."""
        validate_selection(refs)
        if any(ref.part_key is not None for ref in refs):
            raise ValueError("record selection cannot include an attachment")
        archives = [self._lookup(ref, grouped=True) for ref in refs]
        records = []
        for archive in archives:
            record = archive.record
            assert record is not None
            self._project_checked("freeze-records", archive, record)
            records.append(record)
        for record in records[:-1]:
            self._require_record("freeze-records", record)
        return FrozenRecords(tuple(records))

    def read_record(self, batch: FrozenRecords, index: int) -> SelectedRecord:
        archive = self._resolve_record(batch, index)
        assert archive.record is not None
        return self._project_checked("read-record", archive, archive.record)

    def freeze_browser(self, refs: tuple[ObservationRef, ...]) -> FrozenBrowserBatch:
        validate_selection(refs)
        if any(ref.part_key is not None for ref in refs):
            raise ValueError("browser selection cannot include an attachment")
        entries = [self._lookup_browser(ref) for ref in refs]
        for item, event in entries:
            self._read_pair_checked("freeze-browser", item, event)
        for item, _ in entries[:-1]:
            self._require_browser("freeze-browser", item)
        return FrozenBrowserBatch(tuple(item for item, _ in entries))

    def read_projection(self, batch: FrozenBrowserBatch, index: int) -> SelectedProjection:
        item, event = self._resolve_browser(batch, index)
        document = self._read_pair_checked("read-projection", item, event)
        return SelectedProjection(item.ref, date.fromisoformat(event["published_on"]), document)

    def verify_browser(self, batch: FrozenBrowserBatch, index: int) -> None:
        item, event = self._resolve_browser(batch, index)
        self._read_pair_checked("verify-browser", item, event)

    def read_browser_original(self, batch: FrozenBrowserBatch, index: int) -> bytes:
        item, _ = self._resolve_browser(batch, index)
        return self._read_authorized(item.original, self._store, "original_missing",
                                     require=partial(self._require_browser, "read-browser-original", item))

    def restore(self, batch: FrozenBatch, index: int, *, backup: ObjectStore | None) -> Literal["already_present", "restored"]:
        """Restore a missing original from a trusted backup holding the same canonical object key."""
        archive = self._resolve(batch, index)
        return self._restore(archive, backup, partial(self._require, "restore", archive.original))

    def restore_record_original(
        self, batch: FrozenRecords, index: int, *, backup: ObjectStore | None,
    ) -> Literal["already_present", "restored"]:
        """Restore the complete original under a separate operator permission, without returning its bytes."""
        archive = self._resolve_record(batch, index)
        assert archive.record is not None
        return self._restore(archive, backup, partial(self._require_record, "restore-record-original", archive.record))

    def restore_browser_original(
        self, batch: FrozenBrowserBatch, index: int, *, backup: ObjectStore | None,
    ) -> Literal["already_present", "restored"]:
        item, _ = self._resolve_browser(batch, index)
        return self._restore(item.original, backup, partial(self._require_browser, "restore-browser-original", item))

    def restore_browser_projection(
        self, batch: FrozenBrowserBatch, index: int, *, backup: ObjectStore | None,
    ) -> Literal["already_present", "restored"]:
        item, _ = self._resolve_browser(batch, index)
        return self._restore(item.projection, backup, partial(self._require_browser, "restore-browser-projection", item),
                             missing="projection_missing")

    def _restore(self, archive: _ArchivedOriginal | FrozenBrowserObject, backup: ObjectStore | None,
                 require: Callable[[], None], *, missing: str = "original_missing") -> Literal["already_present", "restored"]:
        read = partial(self._read_authorized, archive, require=require)
        try:
            read(self._store, missing)
        except FrozenObjectError as exc:
            if exc.code != missing:
                raise
        else:
            return "already_present"

        if backup is None:
            require()
            raise FrozenObjectError("backup_missing")
        content = read(backup, "backup_missing")
        # A concurrent restore of identical bytes is idempotent under the ObjectStore put contract.
        try:
            self._store.put(archive.artifact_ref, content)
        except (ObjectConflict, OSError) as exc:
            require()
            code = "object_conflict" if isinstance(exc, ObjectConflict) else "store_unavailable"
            raise FrozenObjectError(code) from exc
        read(self._store, missing)
        return "restored"

    def _resolve(self, batch: FrozenBatch, index: int) -> _ArchivedOriginal:
        if not isinstance(batch, FrozenBatch) or type(index) is not int or not 0 <= index < len(batch.items):
            raise ValueError("invalid batch or item index")
        original = batch.items[index]
        archive = self._lookup(original.ref)
        if archive.original != original:
            raise FrozenBatchDenied("access denied")
        return archive

    def _resolve_record(self, batch: FrozenRecords, index: int) -> _ArchivedOriginal:
        if not isinstance(batch, FrozenRecords) or type(index) is not int or not 0 <= index < len(batch.items):
            raise ValueError("invalid record batch or item index")
        record = batch.items[index]
        archive = self._lookup(record.ref, grouped=True)
        if archive.record != record:
            raise FrozenBatchDenied("access denied")
        return archive

    def _resolve_browser(self, batch: FrozenBrowserBatch, index: int) -> tuple[FrozenBrowserObservation, dict]:
        if not isinstance(batch, FrozenBrowserBatch) or type(index) is not int or not 0 <= index < len(batch.items):
            raise ValueError("invalid browser batch or item index")
        item = batch.items[index]
        authoritative, event = self._lookup_browser(item.ref)
        if authoritative != item:
            raise FrozenBatchDenied("access denied")
        return authoritative, event

    def _lookup_browser(self, ref: ObservationRef) -> tuple[FrozenBrowserObservation, dict]:
        with self._db.tx() as conn:
            row = conn.execute(_BROWSER_OBSERVATION, {"source_id": ref.source_id, "notice_key": ref.notice_key,
                                                     "version": ref.observation_version}).one_or_none()
        if row is None:
            raise FrozenBatchDenied("access denied")
        try:
            event = validate_browser_event(row.payload)
            binding = event["binding"]
            if (
                (event["source_id"], event["notice_key"], event["observation_version"])
                != (ref.source_id, ref.notice_key, ref.observation_version)
                or (binding["attempt_id"], binding["run_id"], binding["list_attempt_id"], binding["task_key"])
                != (row.attempt_id, row.run_id, row.list_attempt_id, row.task_key)
                or (event["source_url"], date.fromisoformat(event["published_on"]), instant(event["fetched_at"]))
                != (row.source_url, row.published_on, row.observed_at)
                or (event["content_hash"], event["artifact_ref"], event["size_bytes"])
                != (row.content_hash, row.artifact_ref, row.size_bytes)
            ):
                raise ValueError("browser receipt differs from catalog")
            # The raw artifact can be reused across observations. Media type is pinned by the receipt.
            item = FrozenBrowserObservation.from_receipt(event)
        except (TypeError, ValueError, RecursionError):
            raise FrozenBatchDenied("access denied") from None
        return item, event

    def _lookup(self, ref: ObservationRef, *, grouped: bool = False) -> _ArchivedOriginal:
        with self._db.tx() as conn:
            row = conn.execute(_OBSERVATION, {"source_id": ref.source_id, "notice_key": ref.notice_key,
                                              "version": ref.observation_version,
                                              "part_key": ref.part_key, "grouped": grouped}).one_or_none()
        if row is None:
            raise FrozenBatchDenied("access denied")
        try:
            original = FrozenOriginal(ref, row.content_hash)
            record = FrozenRecord(original, GroupSelector(**row.selector), row.record_digest,
                                  row.published_on) if grouped else None
        except (TypeError, ValueError):
            raise FrozenBatchDenied("access denied") from None
        if row.artifact_ref != original.artifact_ref:
            raise FrozenBatchDenied("access denied")
        return _ArchivedOriginal(original, row.size_bytes, record)

    def _require(self, operation: Operation, original: FrozenOriginal) -> None:
        try:
            allowed = self._authorize is not None and self._authorize(operation, original)
        except Exception as exc:
            raise FrozenBatchDenied("access denied") from exc
        if allowed is not True:
            raise FrozenBatchDenied("access denied")

    def _require_record(self, operation: RecordOperation, record: FrozenRecord) -> None:
        check = partial(self._authorize_record, operation, record) if callable(self._authorize_record) else None
        self._require_scoped(check)

    def _require_browser(self, operation: BrowserOperation, item: FrozenBrowserObservation) -> None:
        check = partial(self._authorize_browser, operation, item) if callable(self._authorize_browser) else None
        self._require_scoped(check)

    @staticmethod
    def _require_scoped(check: Callable[[], bool] | None) -> None:
        context = reqctx.get()
        if check is None or context is None or not context.purpose.strip():
            raise FrozenBatchDenied("access denied")
        try:
            allowed = check()
        except Exception:
            raise FrozenBatchDenied("access denied") from None
        if allowed is not True:
            raise FrozenBatchDenied("access denied")

    def _read_pair_checked(self, operation: BrowserOperation, item: FrozenBrowserObservation, event: dict) -> dict:
        self._require_browser(operation, item)
        try:
            _, document = read_browser_pair(self._store, event)
            return document
        except FileNotFoundError as exc:
            raise FrozenObjectError("browser_object_missing") from exc
        except ValueError as exc:
            raise FrozenObjectError("object_integrity") from exc
        except OSError as exc:
            raise FrozenObjectError("store_unavailable") from exc
        finally:
            self._require_browser(operation, item)

    def _project_checked(self, operation: RecordOperation, archive: _ArchivedOriginal,
                         record: FrozenRecord) -> SelectedRecord:
        self._require_record(operation, record)
        try:
            select = self._record_adapters.get(record.ref.source_id)
            if not callable(select):
                raise FrozenBatchDenied("access denied")
            raw = self._read_object(archive, self._store, "original_missing")
            try:
                html = select(raw, record.selector, published_on=record.published_on,
                              record_digest=record.record_digest)
                if not isinstance(html, str) or not html.strip() or len(html.encode("utf-8")) > MAX_BODY:
                    raise ValueError("invalid selection output")
            except Exception:
                raise FrozenBatchDenied("access denied") from None
            return SelectedRecord(record.ref, record.published_on, html)
        finally:
            self._require_record(operation, record)

    def _read_checked(self, operation: Operation, archive: _ArchivedOriginal, store: ObjectStore,
                      missing: str) -> bytes:
        return self._read_authorized(archive, store, missing, require=partial(self._require, operation, archive.original))

    def _read_authorized(self, archive: _ArchivedOriginal | FrozenBrowserObject, store: ObjectStore, missing: str,
                         *, require: Callable[[], None]) -> bytes:
        require()
        try:
            return self._read_object(archive, store, missing)
        finally:
            require()

    @staticmethod
    def _read_object(archive: _ArchivedOriginal | FrozenBrowserObject, store: ObjectStore, missing: str) -> bytes:
        try:
            if not 0 <= archive.size_bytes <= MAX_BODY:
                raise FrozenObjectError("unsupported_object_size")
            content = store.get(archive.artifact_ref, max_bytes=archive.size_bytes)
            if len(content) != archive.size_bytes or f"sha256:{sha256(content).hexdigest()}" != archive.content_hash:
                raise FrozenObjectError("object_integrity")
        except FileNotFoundError as exc:
            raise FrozenObjectError(missing) from exc
        except ValueError as exc:
            raise FrozenObjectError("object_integrity") from exc
        except OSError as exc:
            raise FrozenObjectError("store_unavailable") from exc
        return content
