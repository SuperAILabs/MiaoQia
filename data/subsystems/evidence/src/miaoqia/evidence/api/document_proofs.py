"""Local evidence owner port for proving exact literals in a pinned document."""
from __future__ import annotations

from dataclasses import dataclass
from collections.abc import Mapping

from sqlalchemy import text

from miaoqia.acquisition.api.browser_pairs import read_browser_pair
from miaoqia.kernel.db import Database
from miaoqia.kernel.objectstore import ObjectStore
from miaoqia.sdk import Document
from miaoqia.sdk.testing import html_to_document
from miaoqia.sdk.validation import validate_extraction

from ..application.bindings import browser_anchor, snapshot_with_binding
from ..application.grouped import MAX_ORIGINAL, GroupedObservation
from ..application.pipeline import HTML_PIPELINE
from ..application.provenance import Provenance
from ..application.queries import load_document
from ..domain.snapshot import normalized_hash, raw_hash
from ..extensions.packs import DocumentAdapter
from .provenance import Closure, ProvenanceIntegrityError, Unbound


@dataclass(frozen=True)
class DocumentSource:
    snapshot_ref: str
    source_id: str
    source_key: str
    raw_sha256: str


class DocumentProofs:
    """Trusted local port. Admit the complete requirements before calling prove.

    Browser and grouped proofs require explicitly supplied owner stores.
    """

    def __init__(self, database: Database, store: ObjectStore, *, grouped_store: ObjectStore | None = None,
                 grouped_adapters: Mapping[str, DocumentAdapter] | None = None,
                 browser_store: ObjectStore | None = None):
        self._database = database
        self._store = store
        self._grouped_store = grouped_store
        self._grouped_adapters = grouped_adapters if grouped_adapters is not None else {}
        self._browser_store = browser_store
        self._provenance = Provenance(database)

    def requirements(self, snapshot_ref: str, evidence_refs: tuple[str, ...]) -> Closure:
        snapshot = self._provenance.snapshot(snapshot_ref)
        closures = [snapshot]
        for ref in evidence_refs:
            proof = self._provenance.evidence(ref)
            if (not set(snapshot.dependencies) <= set(proof.dependencies)
                    or not any(kind == "evidence" and key == ref for kind, key, _ in proof.dependencies)):
                raise ProvenanceIntegrityError("expected document evidence for the fixed snapshot")
            closures.append(proof)
        return Closure.merge(*closures)

    def _snapshot(self, conn, snapshot_ref):
        conn.execute(text("SET TRANSACTION READ ONLY"))
        row = snapshot_with_binding(conn, snapshot_ref)
        if row is None:
            raise Unbound("document snapshot unavailable")
        return row

    @staticmethod
    def _source(row):
        return DocumentSource(row["id"], row["source_id"], row["source_key"], row["raw_sha256"])

    def source(self, snapshot_ref: str) -> DocumentSource:
        with self._database.tx() as conn:
            return self._source(self._snapshot(conn, snapshot_ref))

    def prove(self, snapshot_ref: str, evidence_ref: str, expected: str) -> DocumentSource:
        with self._database.tx() as conn:
            snapshot = self._snapshot(conn, snapshot_ref)
            ref = conn.execute(text("""
                SELECT id, snapshot_id AS snapshot_ref, pipeline_version, locator, quote_hash
                FROM evd_evidence_refs WHERE id = :id AND snapshot_id = :snapshot
            """), {"id": evidence_ref, "snapshot": snapshot_ref}).mappings().one_or_none()
        if ref is None:
            raise Unbound("document evidence unavailable in the fixed snapshot")
        if snapshot["browser_binding"] is not None:
            self._prove_browser(snapshot)
        elif snapshot["grouped_binding"] is not None:
            self._prove_grouped(snapshot)
        else:
            digest = snapshot["raw_sha256"]
            key = snapshot["artifact_ref"]
            if key != "raw/" + digest.removeprefix("sha256:"):
                raise ProvenanceIntegrityError("original object address mismatch")
            if raw_hash(self._store.get(key, max_bytes=MAX_ORIGINAL)) != digest:
                raise ProvenanceIntegrityError("original bytes differ from the fixed snapshot")
        doc = Document(load_document(self._store, snapshot))
        if (doc.pipeline_version != snapshot["pipeline_version"]
                or validate_extraction(doc, [], {evidence_ref: ref}) or doc.quote(ref["locator"]) != expected):
            raise ProvenanceIntegrityError("document locator, quote hash or field value mismatch")
        return self._source(snapshot)

    def _prove_browser(self, snapshot) -> None:
        if self._browser_store is None:
            raise Unbound("browser pair proof unavailable")
        try:
            event = browser_anchor(snapshot)
            _, document = read_browser_pair(self._browser_store, event)
            if (document["pipeline_version"] != snapshot["pipeline_version"]
                    or normalized_hash(document) != snapshot["normalized_hash"]):
                raise ValueError("projection differs from the fixed normalized document")
        except (OSError, ValueError) as exc:
            raise ProvenanceIntegrityError("browser pair differs from the fixed snapshot") from exc

    def _prove_grouped(self, snapshot) -> None:
        adapter = self._grouped_adapters.get(snapshot["source_id"])
        if self._grouped_store is None or adapter is None:
            raise Unbound("grouped original proof unavailable")
        if snapshot["pipeline_version"] != HTML_PIPELINE:
            raise Unbound("grouped document pipeline version unavailable")
        try:
            grouped = GroupedObservation.from_snapshot(snapshot)
            _, html = grouped.read(self._grouped_store, snapshot["artifact_ref"], adapter)
            selected = html_to_document(html, snapshot["id"], pipeline_version=HTML_PIPELINE)
            if normalized_hash(selected) != snapshot["normalized_hash"]:
                raise ValueError("selected member differs from the fixed normalized document")
        except ValueError as exc:
            raise ProvenanceIntegrityError("grouped original or selection differs from the fixed snapshot") from exc
