"""文档证据的本地读取。来源权利判定先于对象读取；拒绝或只引证时不访问正文。

展示策略回调由部署单元注入，通过当前请求上下文核验调用者与用途，缺省拒绝。
本实现不生成原件 URL，不推定来源可达，不把保存快照的时间冒充业务生效时间。
"""
from __future__ import annotations

import json
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


class Resolver:
    def __init__(self, database: db.Database, store: ObjectStore,
                 display: Callable[[tuple[str, ...], str], str] | None = None):
        self.database, self.store, self.display = database, store, display

    def resolve(self, evidence_ref: str) -> ResolvedEvidence | None:
        ctx = reqctx.current()
        ctx.validate()
        if not ctx.purpose or self.display is None:
            raise PermissionError("未装配展示权利判定或缺少用途")
        with self.database.tx() as conn:
            conn.execute(text("SET TRANSACTION READ ONLY"))
            row = conn.execute(text("""SELECT r.id, r.snapshot_id, r.pipeline_version, r.locator, r.quote_hash,
                                               s.source_id, s.source_key, s.rights_tags, s.observed_at, s.document_ref,
                                               s.normalized_hash
                                        FROM evd_evidence_refs r JOIN evd_snapshots s ON s.id = r.snapshot_id
                                        WHERE r.id = :id"""), {"id": evidence_ref}).mappings().one_or_none()
        if row is None:
            return None
        level = self.display(tuple(row["rights_tags"]), ctx.purpose)
        if level not in ("none", "citation_only", "excerpt", "full_text"):
            raise PermissionError("展示策略返回未知级别")
        if level == "none":
            return None
        base = dict(id=evidence_ref, display_level=level, snapshot_ref=row["snapshot_id"], source_id=row["source_id"],
                    source_key=row["source_key"], observed_at=row["observed_at"])
        if level == "citation_only":
            return ResolvedEvidence(**base)
        document = json.loads(self.store.get(row["document_ref"]))
        if normalized_hash(document) != row["normalized_hash"]:
            raise EvidenceIntegrityError("规范化文档与快照哈希不符")
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
