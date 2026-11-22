"""Deterministic search chunks from an immutable, authorized normalized notice."""
from __future__ import annotations

import re

from sqlalchemy import text

from miaoqia.sdk.document import Document

from ..api import Unbound
from ..api.chunks import MAX_NOTICE_CHUNKS, NOTICE_CHUNK_LENGTH, NoticeChunks
from .provenance import Provenance
from .queries import EvidenceIntegrityError, load_document

_SNAPSHOT = re.compile(r"snp_[0-9A-HJKMNP-TV-Z]{26}")


class NoticeChunkReader:
    def __init__(self, database, storage):
        self.database = database
        self.storage = storage
        self.provenance = Provenance(database)

    def read(self, snapshot: str, authorize, *, deadline: float) -> NoticeChunks | None:
        if not isinstance(snapshot, str) or not _SNAPSHOT.fullmatch(snapshot):
            raise ValueError("invalid chunk snapshot")
        try:
            closure = self.provenance.snapshot(snapshot, deadline=deadline)
        except Unbound:
            return None
        if authorize(closure.sources) is not True:
            return None
        with self.database.tx(deadline=deadline) as conn:
            row = conn.execute(text("""
                SELECT id, pipeline_version, document_ref, normalized_hash
                FROM evd_snapshots WHERE id=:id
            """), {"id": snapshot}).mappings().one_or_none()
        if row is None:
            raise EvidenceIntegrityError("chunk snapshot unavailable")
        doc = Document(load_document(self.storage, row, deadline=deadline))
        if doc.snapshot_ref != snapshot or doc.pipeline_version != row["pipeline_version"]:
            raise EvidenceIntegrityError("chunk document identity differs from snapshot")
        spans = []
        for unit in doc.units():
            for start in range(0, len(unit.text), NOTICE_CHUNK_LENGTH):
                span = unit.span(start, min(start + NOTICE_CHUNK_LENGTH, len(unit.text)))
                if span.text.strip():
                    spans.append(span)
                if len(spans) == MAX_NOTICE_CHUNKS:
                    return NoticeChunks(snapshot, tuple(spans))
        return NoticeChunks(snapshot, tuple(spans))
