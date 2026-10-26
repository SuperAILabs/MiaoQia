"""公共实例的文档管线：原始制品 → 规范化文档 → 快照判定 → 解析编排 → 字段证据（evidence.md 第3节）。

一次 ingest 在一个数据库事务内完成快照、解析修订、证据引用与发件箱事件，事件与数据一致；对象存储写入先于事务，
对象不可变且按内容寻址（原件）或按快照（规范化文档），事务回滚只会留下无人引用的对象，不会改写已发布对象。

同一来源对象的规范化内容与最新快照相同时不产生新快照（回到历史内容仍是新快照）；同一快照在同一解析器版本下只解析一次，重复投放幂等，不重复发布事件。
本管线只处理公共来源：租户上下文下调用被拒绝（租户文档属于租户实例，见 evidence.md 第11节）。
"""
from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime

from sqlalchemy import Connection, text

from miaoqia.kernel import ids, outbox, reqctx
from miaoqia.sdk import Document, ExtractionContext
from miaoqia.sdk.testing import html_to_document
from miaoqia.sdk.validation import validate_extraction

from ..api import EXTRACTION_REVISED, NAME, SNAPSHOT_CREATED
from ..domain.snapshot import normalized_hash, quality_status, raw_hash
from ..extensions.packs import PackExtractor
from ..infrastructure.objectstore import ObjectStore

HTML_PIPELINE = "doc.html-dev@0.1.0"     # SDK 的 HTML 规范化（与语料评测同一坐标系）；PDF/OCR 管线随 M1 交付

writer = outbox.Writer(subsystem=NAME, namespace=NAME, zone=reqctx.PUBLIC)


class TenantScopeRejected(PermissionError):
    """公共管线不接受租户上下文。"""


@dataclass(frozen=True)
class Artifact:
    """一份原始制品：来源与来源对象键、媒体类型、原件字节、权利标签与观察时间。

    normalized 只用于原件没有可用文本层的情形（如扫描 PDF），必须是经复核的规范化表示，其 snapshot_ref 会被替换。
    """

    source_id: str
    source_key: str
    media_type: str
    raw: bytes
    rights_tags: tuple[str, ...]
    observed_at: datetime
    source_url: str | None = None
    normalized: dict | None = None


@dataclass(frozen=True)
class Ingested:
    snapshot_ref: str
    snapshot_created: bool
    revisions: tuple[str, ...]          # 本次新建的解析修订
    existing_revisions: tuple[str, ...]  # 已存在、未重复解析的修订


class Pipeline:
    def __init__(self, store: ObjectStore, extractors: list[PackExtractor]):
        self.store = store
        self.extractors = extractors

    def _normalize(self, art: Artifact) -> dict:
        placeholder = "snp_" + "0" * 26
        if art.normalized is not None:
            return {**art.normalized, "snapshot_ref": placeholder}
        if art.media_type != "text/html":
            raise ValueError(f"不支持的媒体类型：{art.media_type}（PDF 需提供经复核的规范化表示）")
        return html_to_document(art.raw.decode("utf-8"), placeholder, art.source_url, pipeline_version=HTML_PIPELINE)

    def ingest(self, conn: Connection, art: Artifact) -> Ingested:
        ctx = reqctx.current()
        if ctx.tenant or ctx.cell:
            raise TenantScopeRejected("公共文档管线不处理租户上下文")
        if not art.rights_tags or len(set(art.rights_tags)) != len(art.rights_tags):
            raise ValueError("原始制品必须带不重复的权利标签")
        document = self._normalize(art)
        digest = normalized_hash(document)
        raw_digest = raw_hash(art.raw)
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": f"{art.source_id}|{art.source_key}"})
        # 与来源对象的最新快照比较（不是全部历史）：内容回到旧值也是来源的一次变化，须产生新快照并驱动物化。
        latest = conn.execute(text("""SELECT id, normalized_hash FROM evd_snapshots WHERE source_id = :s AND source_key = :k
                                      ORDER BY seq DESC LIMIT 1"""), {"s": art.source_id, "k": art.source_key}).first()
        row = latest if latest is not None and latest[1] == digest else None
        created = row is None
        if created:
            snapshot = ids.new("snp")
            document["snapshot_ref"] = snapshot
            artifact_ref = f"raw/{raw_digest.removeprefix('sha256:')}"
            document_ref = f"normalized/{snapshot}.json"
            self.store.put(artifact_ref, art.raw)
            self.store.put(document_ref, json.dumps(document, ensure_ascii=False, sort_keys=True).encode("utf-8"))
            seq = conn.execute(text("SELECT coalesce(max(seq), 0) + 1 FROM evd_snapshots WHERE source_id = :s AND source_key = :k"),
                               {"s": art.source_id, "k": art.source_key}).scalar_one()
            conn.execute(text("""
                INSERT INTO evd_snapshots (id, source_id, source_key, seq, normalized_hash, raw_sha256, artifact_ref, document_ref,
                                           media_type, pipeline_version, rights_tags, observed_at)
                VALUES (:id, :s, :k, :seq, :h, :raw, :a, :d, :m, :p, CAST(:r AS jsonb), :o)"""),
                {"id": snapshot, "s": art.source_id, "k": art.source_key, "seq": seq, "h": digest, "raw": raw_digest,
                 "a": artifact_ref, "d": document_ref, "m": art.media_type, "p": document["pipeline_version"],
                 "r": json.dumps(list(art.rights_tags)), "o": art.observed_at})
            writer.append(conn, outbox.Event(type=SNAPSHOT_CREATED, subject=snapshot, partition_key=f"{art.source_id}|{art.source_key}",
                                             data={"snapshot_ref": snapshot, "source_id": art.source_id, "artifact_ref": artifact_ref,
                                                   "rights_tags": list(art.rights_tags), "content_hash": raw_digest}))
        else:
            snapshot = row[0]
            document["snapshot_ref"] = snapshot
        doc = Document(document)
        source = {"source_id": art.source_id, "source_url": art.source_url}
        new, existing = [], []
        for pe in self.extractors:
            if not pe.extractor.applies(doc, source):
                continue
            prior = conn.execute(text("SELECT id FROM evd_extraction_revisions WHERE snapshot_id = :s AND extractor = :e"),
                                 {"s": snapshot, "e": pe.ref}).first()
            if prior is not None:
                existing.append(prior[0])
                continue
            new.append(self._extract(conn, doc, art, pe))
        return Ingested(snapshot, created, tuple(new), tuple(existing))

    def _extract(self, conn: Connection, doc: Document, art: Artifact, pe: PackExtractor) -> str:
        ectx = ExtractionContext(doc, pe.ref)
        result = pe.extractor.extract(doc, ectx)
        errors = validate_extraction(doc, result.facts, ectx.evidence, semantic_validator=pe.semantic_validator)
        status = quality_status(errors, result.warnings, result.facts)
        warnings = list(result.warnings) + [{"code": "VALIDATION_FAILED", "message": e} for e in errors]
        revision = ids.new("exr")
        conn.execute(text("""
            INSERT INTO evd_extraction_revisions (id, snapshot_id, extractor, pack_id, pack_version, facts, quality_status, warnings)
            VALUES (:id, :s, :e, :pid, :pv, CAST(:f AS jsonb), :q, CAST(:w AS jsonb))"""),
            {"id": revision, "s": doc.snapshot_ref, "e": pe.ref, "pid": pe.pack_id, "pv": pe.pack_version,
             "f": json.dumps(result.facts, ensure_ascii=False), "q": status, "w": json.dumps(warnings, ensure_ascii=False)})
        for ref in ectx.evidence.values():
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
