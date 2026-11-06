"""文档证据的本地读取。来源权利判定先于对象读取；拒绝或只引证时不访问正文。

展示策略回调由部署单元注入，通过当前请求上下文核验调用者与用途，缺省拒绝。
本实现不生成原件 URL，不推定来源可达，不把保存快照的时间冒充业务生效时间。
"""
from __future__ import annotations

import json
import inspect
from collections.abc import Callable

from sqlalchemy import text

from miaoqia.kernel import db, reqctx
from miaoqia.sdk import Document
from miaoqia.sdk.validation import validate_extraction

from ..api import ResolvedEvidence
from ..domain.snapshot import normalized_hash
from ..infrastructure.objectstore import ObjectStore


class EvidenceIntegrityError(ValueError):
    """已发布证据与对象存储不一致。"""

OBJECT_READ_MAX = 4 * 1024 * 1024


def load_document(store, row, *, deadline=None):
    """唯一规范化对象完整性检查；调用者必须先判定当前正文读取权利。"""
    getter = store.get
    params = inspect.signature(getter).parameters.values()
    if any(p.kind == inspect.Parameter.VAR_KEYWORD for p in params) or {p.name for p in params} >= {"key", "max_bytes", "deadline"}:
        raw_document = getter(row["document_ref"], max_bytes=OBJECT_READ_MAX, deadline=deadline)
    else:
        # Preserve compatibility with test doubles and older adapters while the port evolves.
        raw_document = getter(row["document_ref"])
    document = json.loads(raw_document)
    if normalized_hash(document) != row["normalized_hash"]:
        raise EvidenceIntegrityError("规范化文档与快照哈希不符")
    return document


class Resolver:
    def __init__(self, database: db.Database, store: ObjectStore,
                 display: Callable[[tuple[str, ...], str], str] | None = None, *, enrichments=None, authorize=None):
        if display is not None and authorize is not None:
            raise ValueError("choose one authorization mode")
        from .provenance import Provenance
        self.provenance, self.authorize = Provenance(database), authorize
        self.database, self.store, self.display = database, store, display
        self.enrichments = enrichments

    def resolve(self, evidence_ref: str, *, deadline=None) -> ResolvedEvidence | None:
        ctx = reqctx.current()
        ctx.validate()
        if not ctx.purpose or (self.display is None and self.authorize is None):
            raise PermissionError("未装配展示权利判定或缺少用途")
        with self.database.tx(deadline=deadline) as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            row = conn.execute(text("""SELECT r.id, r.snapshot_id, r.pipeline_version, r.locator, r.quote_hash,
                                               s.source_id, s.source_key, s.rights_tags, s.observed_at, s.document_ref,
                                               s.normalized_hash
                                        FROM evd_evidence_refs r JOIN evd_snapshots s ON s.id = r.snapshot_id
                                        WHERE r.id = :id"""), {"id": evidence_ref}).mappings().one_or_none()
        if row is None:
            return self.enrichments.resolve(evidence_ref) if self.enrichments else None
        level = self._level(tuple(row["rights_tags"]), lambda: self.provenance.evidence(evidence_ref))
        if level == "none":
            return None
        base = dict(id=evidence_ref, display_level=level, snapshot_ref=row["snapshot_id"], source_id=row["source_id"],
                    source_key=row["source_key"], observed_at=row["observed_at"])
        if level == "citation_only":
            return ResolvedEvidence(**base)
        document = load_document(self.store, row, deadline=deadline)
        doc = Document(document)
        ref = {"id": row["id"], "snapshot_ref": row["snapshot_id"], "pipeline_version": row["pipeline_version"],
               "locator": row["locator"], "quote_hash": row["quote_hash"]}
        if validate_extraction(doc, [], {evidence_ref: ref}):
            raise EvidenceIntegrityError("证据归属、定位或原文哈希不符")
        quote = doc.quote(row["locator"])
        context = None
        if level == "full_text":
            loc = row["locator"]
            context = "\n".join(u.text for u in doc.units() if loc["block"] <= u.block <= loc.get("block_end", loc["block"]))
        return ResolvedEvidence(**base, locator=row["locator"], snippet=quote if level == "full_text" else quote[:512], context=context)

    def _level(self, tags, closure):
        ctx = reqctx.current()
        ctx.validate()
        if not ctx.purpose or (self.display is None and self.authorize is None):
            raise PermissionError("未装配展示权利判定或缺少用途")
        if self.authorize is not None:
            from ..api import Unbound
            try:
                groups = self.provenance.resolve(closure())
            except Unbound:
                return "none"
            level = self.authorize(groups, ctx.purpose)
        else:
            level = self.display(tags, ctx.purpose)
        if level not in ("none", "citation_only", "excerpt", "full_text"):
            raise PermissionError("展示策略返回未知级别")
        return level

    def snapshot(self, snapshot_ref):
        """Owner full read; reject before reading either object when display is clipped."""
        import hashlib
        from ..api import SnapshotRead
        # Admission runs even for an absent handle.
        self._level((), lambda: self.provenance.snapshot(snapshot_ref))
        with self.database.tx() as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            row = conn.execute(text("SELECT * FROM evd_snapshots WHERE id=:i"),
                               {"i": snapshot_ref}).mappings().one_or_none()
        if row is None or self._level(tuple(row["rights_tags"]), lambda: self.provenance.snapshot(snapshot_ref)) != "full_text":
            return None
        raw = self.store.get(row["artifact_ref"])
        if "sha256:" + hashlib.sha256(raw).hexdigest() != row["raw_sha256"]:
            raise EvidenceIntegrityError("原件与快照哈希不符")
        return SnapshotRead(row["id"], row["source_id"], row["source_key"], tuple(row["rights_tags"]),
                            row["raw_sha256"], raw, load_document(self.store, row))
