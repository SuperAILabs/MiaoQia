"""公共实例的文档管线：原始制品 → 规范化文档 → 快照判定 → 解析编排 → 字段证据（evidence.md 第3节）。

一次 ingest 在一个数据库事务内完成快照、解析修订、证据引用与发件箱事件，事件与数据一致；对象存储写入先于事务，
对象不可变且按内容寻址（原件 raw/<sha256>）或按管线版本与快照（规范化文档 normalized/<管线>/<版本>/<快照>.json），事务回滚只会留下无人引用的对象，不会改写已发布对象。

同一来源对象的规范化内容与最新快照相同时不产生新快照（回到历史内容仍是新快照）；同一快照在同一解析器版本下只解析一次，重复投放幂等，不重复发布事件。
本管线只处理公共来源：租户上下文下调用被拒绝（租户文档属于租户实例，见 evidence.md 第11节）。
"""
from __future__ import annotations

import json
from dataclasses import asdict, dataclass
from datetime import datetime

from sqlalchemy import Connection, text

from miaoqia.acquisition.api.browser_pairs import read_browser_pair
from miaoqia.kernel import ids, outbox, reqctx
from miaoqia.kernel.objectstore import ObjectStore, versioned
from miaoqia.sdk import Document, ExtractionContext
from miaoqia.sdk.testing import html_to_document
from miaoqia.sdk.validation import evidence_links, validate_extraction

from ..api import EXTRACTION_REVISED, NAME, SNAPSHOT_CREATED
from ..domain.snapshot import OBJECT_READ_MAX, normalized_hash, quality_status, raw_hash
from ..extensions.packs import PackExtractor
from .bindings import bind_browser_observation, lock_snapshot_stream
from .grouped import GroupedObservation

HTML_PIPELINE = "doc.html-dev@0.1.0"     # SDK 的 HTML 规范化（与语料评测同一坐标系）；PDF/OCR 管线随 M1 交付

writer = outbox.Writer(subsystem=NAME, namespace=NAME, zone=reqctx.PUBLIC)


class TenantScopeRejected(PermissionError):
    """公共管线不接受租户上下文。"""


@dataclass(frozen=True)
class Attachment:
    part_key: str
    required: bool
    status: str
    name: str | None = None
    artifact_ref: str | None = None
    content_hash: str | None = None
    media_type: str | None = None
    size_bytes: int | None = None


@dataclass(frozen=True)
class Artifact:
    """一份原始制品：来源与来源对象键、媒体类型、原件字节、权利标签与观察时间。

    normalized 用于经复核的规范化表示（如扫描 PDF）和已验证的浏览器惰性投影，其 snapshot_ref 会被替换。
    html 用于原件不是 HTML、正文由领域包的文档适配器从原件取出的情形（如接口返回的 JSON）；原件仍按原样归档。
    """

    source_id: str
    source_key: str
    media_type: str
    raw: bytes
    rights_tags: tuple[str, ...]
    observed_at: datetime
    source_url: str | None = None
    normalized: dict | None = None
    html: str | None = None
    attachments: tuple[Attachment, ...] = ()


@dataclass(frozen=True)
class Ingested:
    snapshot_ref: str
    snapshot_created: bool
    revisions: tuple[str, ...]          # 本次新建的解析修订
    existing_revisions: tuple[str, ...]  # 已存在、未重复解析的修订


def browser_artifact(archive: ObjectStore, event: dict, observed_at: datetime) -> Artifact:
    raw, projection = read_browser_pair(archive, event)
    return Artifact(
        source_id=event["source_id"], source_key=f"notice:{event['notice_key']}",
        media_type=event["media_type"], raw=raw, rights_tags=tuple(event["rights_tags"]),
        observed_at=observed_at, source_url=event["source_url"], normalized=projection,
    )


class Pipeline:
    def __init__(self, store: ObjectStore, extractors: list[PackExtractor]):
        self.store = store
        self.extractors = extractors

    def _normalize(self, art: Artifact) -> dict:
        placeholder = "snp_" + "0" * 26
        if art.normalized is not None:
            return {**art.normalized, "snapshot_ref": placeholder}
        if art.html is not None:
            return html_to_document(art.html, placeholder, art.source_url, pipeline_version=HTML_PIPELINE)
        if art.media_type != "text/html":
            raise ValueError(f"不支持的媒体类型：{art.media_type}（PDF 需提供经复核的规范化表示）")
        return html_to_document(art.raw.decode("utf-8"), placeholder, art.source_url, pipeline_version=HTML_PIPELINE)

    def ingest(self, conn: Connection, art: Artifact, *, archived_ref: str | None = None,
               grouped: GroupedObservation | None = None) -> Ingested:
        """Ingest an offline artifact or a live artifact already verified against its archive."""
        return self._ingest(conn, art, archived_ref=archived_ref, grouped=grouped)

    def ingest_browser(self, conn: Connection, event: dict, observed_at: datetime,
                       archive: ObjectStore) -> Ingested:
        """Materialize the archive pair after LiveIngest has admitted its source and version."""
        artifact = browser_artifact(archive, event, observed_at)
        return self._ingest(conn, artifact, archived_ref=event["artifact_ref"], browser=event)

    def assess(self, art: Artifact, *, selected: bool) -> dict:
        """Evaluate the document and parser revisions without publishing artifacts or facts."""
        document, _ = self._document(art)
        if selected:
            self._document_bytes(document, selected=True)
        digest = normalized_hash(document)
        doc = Document(document)
        revisions = []
        for extractor in self._applicable(doc, art):
            result, evidence, errors, status = self._evaluate(doc, art, extractor)
            fact_evidence_count = 0
            if status != "suspect":
                fact_evidence_count = len({ref for _, ref in evidence_links(result.facts)})
            revisions.append({
                "extractor": extractor.ref, "pack_id": extractor.pack_id, "pack_version": extractor.pack_version,
                "quality_status": status, "fact_count": len(result.facts), "evidence_count": len(evidence),
                "fact_evidence_count": fact_evidence_count, "warning_count": len(result.warnings),
                "validation_error_count": len(errors),
            })
        if any(revision["fact_evidence_count"] for revision in revisions):
            field_evidence = "available"
        elif revisions:
            field_evidence = "no_valid_evidence"
        else:
            field_evidence = "no_matching_extractor"
        return {"normalized_hash": digest, "pipeline_version": doc.pipeline_version,
                "field_evidence": field_evidence, "revisions": revisions}

    def _document(self, art: Artifact) -> tuple[dict, list[dict]]:
        if not art.rights_tags or len(set(art.rights_tags)) != len(art.rights_tags):
            raise ValueError("原始制品必须带不重复的权利标签")
        document = self._normalize(art)
        attachments = sorted(art.attachments, key=lambda part: part.part_key)
        if len({part.part_key for part in attachments}) != len(attachments):
            raise ValueError("duplicate attachment keys")
        if attachments:
            document["attachments"] = [
                {"name": part.name or part.part_key, **({"media_type": part.media_type} if part.media_type else {})}
                for part in attachments
            ]
        return document, [asdict(part) for part in attachments]

    @staticmethod
    def _document_bytes(document: dict, *, selected: bool) -> bytes:
        encoded = json.dumps(document, ensure_ascii=False, sort_keys=True).encode("utf-8")
        if selected and len(encoded) > OBJECT_READ_MAX:
            raise ValueError("selected normalized document exceeds the evidence read limit")
        return encoded

    def _ingest(self, conn: Connection, art: Artifact, *, archived_ref: str | None,
                grouped: GroupedObservation | None = None, browser: dict | None = None) -> Ingested:
        ctx = reqctx.current()
        if ctx.tenant or ctx.cell:
            raise TenantScopeRejected("公共文档管线不处理租户上下文")
        if grouped is not None and (archived_ref is None or art.html is None or art.normalized is not None
                                    or art.attachments or art.source_url is not None or art.source_id != grouped.source_id
                                    or art.source_key != f"notice:{grouped.notice_key}"
                                    or art.media_type != grouped.media_type or len(art.raw) != grouped.size_bytes):
            raise ValueError("invalid grouped pipeline input")
        document, receipts = self._document(art)
        digest = normalized_hash(document)
        raw_digest = raw_hash(art.raw)
        artifact_ref = f"raw/{raw_digest.removeprefix('sha256:')}"
        if archived_ref is not None and archived_ref != artifact_ref:
            raise ValueError("verified archive reference does not match the original bytes")
        if grouped is not None and grouped.raw_sha256 != raw_digest:
            raise ValueError("grouped pipeline input does not match the original bytes")
        # 与来源对象的最新快照比较（不是全部历史）：内容回到旧值也是来源的一次变化，须产生新快照并驱动物化。
        latest = lock_snapshot_stream(conn, art.source_id, art.source_key,
                                      browser_url=browser["source_url"] if browser is not None else None)
        row = latest if latest is not None and latest.normalized_hash == digest else None
        if row is not None and ((row.grouped_binding is None) != (grouped is None)
                                or (grouped is not None and (
                                    not grouped.same_member(row.grouped_binding)
                                    or set(row.rights_tags) != set(art.rights_tags)))):
            row = None
        if row is not None and browser is not None:
            pinned = row.browser_binding
            if (any(pinned[field] != browser[field] for field in (
                    "artifact_ref", "projection", "published_on", "media_type", "content_encoding", "source_url"))
                    or set(row.rights_tags) != set(art.rights_tags)):
                row = None
        if row is not None and receipts:
            prior_receipts = conn.execute(text("""
                SELECT receipts FROM evd_snapshot_attachments WHERE snapshot_id = :id
            """), {"id": row.id}).scalar_one_or_none()
            if prior_receipts != receipts:
                row = None
        created = row is None
        if created:
            snapshot = ids.new("snp")
            document["snapshot_ref"] = snapshot
            document_ref = f"normalized/{versioned(document['pipeline_version'])}/{snapshot}.json"
            document_bytes = self._document_bytes(document, selected=grouped is not None or browser is not None)
            if archived_ref is None:
                self.store.put(artifact_ref, art.raw)
            self.store.put(document_ref, document_bytes)
            seq = conn.execute(text("SELECT coalesce(max(seq), 0) + 1 FROM evd_snapshots WHERE source_id = :s AND source_key = :k"),
                               {"s": art.source_id, "k": art.source_key}).scalar_one()
            conn.execute(text("""
                INSERT INTO evd_snapshots (id, source_id, source_key, seq, normalized_hash, raw_sha256, artifact_ref, document_ref,
                                           media_type, pipeline_version, rights_tags, observed_at)
                VALUES (:id, :s, :k, :seq, :h, :raw, :a, :d, :m, :p, CAST(:r AS jsonb), :o)"""),
                {"id": snapshot, "s": art.source_id, "k": art.source_key, "seq": seq, "h": digest, "raw": raw_digest,
                 "a": artifact_ref, "d": document_ref, "m": art.media_type, "p": document["pipeline_version"],
                 "r": json.dumps(list(art.rights_tags)), "o": art.observed_at})
            if receipts:
                conn.execute(text("""
                    INSERT INTO evd_snapshot_attachments (snapshot_id, receipts) VALUES (:id, CAST(:parts AS jsonb))
                """), {"id": snapshot, "parts": json.dumps(receipts)})
            writer.append(conn, outbox.Event(type=SNAPSHOT_CREATED, subject=snapshot, partition_key=f"{art.source_id}|{art.source_key}",
                                             data={"snapshot_ref": snapshot, "source_id": art.source_id, "artifact_ref": artifact_ref,
                                                   "rights_tags": list(art.rights_tags), "content_hash": raw_digest}))
        else:
            snapshot = row.id
            document["snapshot_ref"] = snapshot
        if grouped is not None:
            grouped.bind(conn, snapshot, is_anchor=created)
        if browser is not None:
            bind_browser_observation(conn, browser, snapshot, is_anchor=created)
        doc = Document(document)
        new, existing = [], []
        for pe in self._applicable(doc, art):
            prior = conn.execute(text("SELECT id FROM evd_extraction_revisions WHERE snapshot_id = :s AND extractor = :e"),
                                 {"s": snapshot, "e": pe.ref}).first()
            if prior is not None:
                existing.append(prior[0])
                continue
            new.append(self._extract(conn, doc, art, pe))
        return Ingested(snapshot, created, tuple(new), tuple(existing))

    def _applicable(self, doc: Document, art: Artifact):
        source = {"source_id": art.source_id, "source_url": art.source_url}
        return (pe for pe in self.extractors if pe.extractor.applies(doc, source))

    @staticmethod
    def _evaluate(doc: Document, art: Artifact, pe: PackExtractor):
        ectx = ExtractionContext(doc, pe.ref)
        result = pe.extractor.extract(doc, ectx)
        if any(part.required and part.status == "pending" for part in art.attachments):
            result.warnings.append({"code": "REQUIRED_ATTACHMENT_PENDING", "message": "来源声明的必需附件待补采"})
        errors = validate_extraction(doc, result.facts, ectx.evidence, semantic_validator=pe.semantic_validator)
        status = quality_status(errors, result.warnings, result.facts)
        return result, ectx.evidence, errors, status

    def _extract(self, conn: Connection, doc: Document, art: Artifact, pe: PackExtractor) -> str:
        result, evidence, errors, status = self._evaluate(doc, art, pe)
        warnings = list(result.warnings) + [{"code": "VALIDATION_FAILED", "message": e} for e in errors]
        revision = ids.new("exr")
        conn.execute(text("""
            INSERT INTO evd_extraction_revisions (id, snapshot_id, extractor, pack_id, pack_version, facts, quality_status, warnings)
            VALUES (:id, :s, :e, :pid, :pv, CAST(:f AS jsonb), :q, CAST(:w AS jsonb))"""),
            {"id": revision, "s": doc.snapshot_ref, "e": pe.ref, "pid": pe.pack_id, "pv": pe.pack_version,
             "f": json.dumps(result.facts, ensure_ascii=False), "q": status, "w": json.dumps(warnings, ensure_ascii=False)})
        for ref in evidence.values():
            stored = conn.execute(text("""
                INSERT INTO evd_evidence_refs (id, snapshot_id, pipeline_version, locator, quote_hash)
                VALUES (:id, :s, :p, CAST(:l AS jsonb), :q) ON CONFLICT (id) DO NOTHING RETURNING id"""),
                {"id": ref["id"], "s": ref["snapshot_ref"], "p": ref["pipeline_version"],
                 "l": json.dumps(ref["locator"], sort_keys=True), "q": ref["quote_hash"]}).first()
            if stored is None:                       # 已由同一快照的其他修订登记：内容必须一致
                same = conn.execute(text("""SELECT snapshot_id = :s AND pipeline_version = :p AND locator = CAST(:l AS jsonb) AND quote_hash = :q
                                            FROM evd_evidence_refs WHERE id = :id"""),
                                    {"id": ref["id"], "s": ref["snapshot_ref"], "p": ref["pipeline_version"],
                                     "l": json.dumps(ref["locator"], sort_keys=True), "q": ref["quote_hash"]}).scalar_one()
                if not same:
                    raise ValueError(f"证据引用冲突：{ref['id']}")
        writer.append(conn, outbox.Event(type=EXTRACTION_REVISED, subject=revision, partition_key=f"{art.source_id}|{art.source_key}",
                                         data={"snapshot_ref": doc.snapshot_ref, "revision_ref": revision,
                                               "rights_tags": list(art.rights_tags), "pack_id": pe.pack_id,
                                               "pack_version": pe.pack_version, "quality_status": status}))
        return revision
