"""领域事实物化：消费 evidence.extraction.revised.v1，把解析修订中的领域字段写成领域事实的新版本，并经发件箱发布
domain.fact.upserted.v1；冻结来源的项目聚合及首批 tendering 事件也在同一事务中。

- 只物化公共来源、已登记信息领域与对象类型的修订；质量为 suspect（证据或领域语义校验不通过）的修订进入复核，不物化；
- 取值（去掉溯源与部分标识）与当前版本相同时不产生新版本、不发布事件；同一修订重复投递幂等；
- 新版本补齐公共字段（public_fact_meta）后按契约模型完整校验，校验不通过时事务失败（消费框架重试并转入死信），
  不写入不合契约的事实；
- 同一来源对象的事实按来源对象加锁，版本号连续递增，不会因并发产生同号版本。
"""
from __future__ import annotations

import copy
import json
import logging
from datetime import datetime, timezone

from sqlalchemy import Connection, text

from miaoqia.contracts import models
from miaoqia.kernel import ids, outbox, payloads, reqctx

from ..api import FACT_UPSERTED, NAME
from ..domain.versions import changed_fields, stabilize_result_ids, value_hash

log = logging.getLogger("miaoqia.domain_intelligence.facts")

EXTRACTION_REVISED = "evidence.extraction.revised.v1"

# 信息领域 → (对象类型, 标识前缀, 契约模型, Schema 版本)。Schema 版本与 contracts/schemas 的 x-version 一致（测试核对）。
OBJECT_TYPES = {
    "INF-PROC": ("tendering.notice", "ntc", models.Notice, "1.2.0"),
    "INF-ORG": ("organization.registration", "ogr", models.OrgRegistration, "1.2.0"),
}

writer = outbox.Writer(subsystem=NAME, namespace="domain", zone=reqctx.PUBLIC)


def _iso(t: datetime) -> str:
    return t.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


class Materializer:
    def __init__(self, extractions):
        self.extractions = extractions          # evidence api.Extractions

    def on_extraction_revised(self, conn: Connection, env: dict) -> None:
        if env.get("miaoqiazone") != reqctx.PUBLIC or env.get("source") != "miaoqia/evidence":
            raise ValueError("解析修订事件必须来自公共 evidence 实例")
        data = env["data"]
        if data["pack_id"] not in OBJECT_TYPES:
            return
        if data["quality_status"] == "suspect":
            log.info("解析修订待复核，不物化 revision=%s", data["revision_ref"])
            return
        revision = self.extractions.revision(data["revision_ref"])
        if revision is None:
            raise LookupError(f"解析修订不存在：{data['revision_ref']}")       # 重试：读取方可能落后于事件
        if (revision.snapshot_ref != data["snapshot_ref"] or revision.pack_id != data["pack_id"]
                or revision.quality_status != data["quality_status"] or list(revision.rights_tags) != data["rights_tags"]):
            raise ValueError("事件与解析修订不一致")
        self.materialize(conn, revision)

    def materialize(self, conn: Connection, revision) -> list[dict]:
        object_type, prefix, model, schema_version = OBJECT_TYPES[revision.pack_id]
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"),
                     {"k": f"{revision.pack_id}|{revision.source_id}|{revision.source_key}"})
        # 多事实修订先按稳定顺序锁住全部身份；避免来源A/B以相反顺序竞争两个项目。
        if revision.pack_id == "INF-PROC":
            from .projects import lock_identities
            lock_identities(conn, revision)
        published = []
        for ordinal, extracted in enumerate(revision.facts):
            row = conn.execute(text("""SELECT f.id, f.current_version, v.content FROM dom_facts f
                                       JOIN dom_fact_versions v ON v.fact_id = f.id AND v.version = f.current_version
                                       WHERE f.info_domain = :d AND f.object_type = :t AND f.source_id = :s AND f.source_key = :k
                                         AND f.ordinal = :o"""),
                               {"d": revision.pack_id, "t": object_type, "s": revision.source_id, "k": revision.source_key,
                                "o": ordinal}).first()
            fact_id, version, prev = (row[0], row[1], row[2]) if row else (ids.new(prefix), 0, None)
            if prev is not None and prev["revision_ref"] == revision.id:
                continue                                   # 同一修订已物化
            if conn.execute(text("SELECT 1 FROM dom_fact_versions WHERE fact_id = :f AND revision_ref = :r"),
                            {"f": fact_id, "r": revision.id}).first():
                continue
            fact = stabilize_result_ids(prev, copy.deepcopy(extracted))
            full = write_version(conn, revision, fact, fact_id=fact_id, version=version, prev=prev,
                                 object_type=object_type, model=model, schema_version=schema_version,
                                 source_id=revision.source_id, source_key=revision.source_key, ordinal=ordinal)
            if full is not None:
                if revision.pack_id == "INF-PROC":
                    from .projects import aggregate_notice
                    aggregate_notice(conn, revision, full)
                published.append(full)
        return published


def write_version(conn, revision, fact, *, fact_id, version, prev, object_type, model, schema_version,
                  source_id, source_key, ordinal=0, derivation_key="base", rights_tags=None, compare_evidence=False, source_closure=None, defer_closure=False):
    """共用事实版本写入；调用者先持有来源/聚合事务锁。"""
    rights_tags = list(revision.rights_tags) if rights_tags is None else rights_tags
    digest = value_hash(fact)
    if compare_evidence:
        from miaoqia.sdk.derivation import fingerprint
        from ..domain.versions import META
        body = {k: v for k, v in fact.items() if k not in META}
        old_body = {k: v for k, v in (prev or {}).items() if k not in META}
        fields = ["/" + k.replace("~", "~0").replace("/", "~1") for k in sorted(set(body) | set(old_body))
                  if body.get(k) != old_body.get(k)]
        if prev and prev["rights_tags"] != rights_tags:
            fields.append("/rights_tags")
        if prev is not None and not fields:
            return None
        digest = fingerprint({"body": body, "rights_tags": rights_tags})
    else:
        if prev is not None and value_hash(prev) == digest:
            return None
        fields = changed_fields(prev, fact)
    info_domain = revision.pack_id
    now = datetime.now(timezone.utc)
    full = {**fact, "id": fact_id, "schema_version": schema_version, "version": version + 1,
            "info_domain": info_domain, "snapshot_ref": revision.snapshot_ref, "revision_ref": revision.id,
            "rights_tags": rights_tags, "classification": "public",
            "observed_at": _iso(revision.observed_at), "recorded_at": _iso(now)}
    model.model_validate(full)                      # 契约校验；失败即事务失败
    if prev is None:
        conn.execute(text("""INSERT INTO dom_facts (id, info_domain, object_type, source_id, source_key, ordinal, current_version)
                             VALUES (:id, :d, :t, :s, :k, :o, 1)"""),
                     {"id": fact_id, "d": info_domain, "t": object_type, "s": source_id,
                      "k": source_key, "o": ordinal})
    else:
        conn.execute(text("UPDATE dom_facts SET current_version = :v, updated_at = now() WHERE id = :id"),
                     {"v": version + 1, "id": fact_id})
    conn.execute(text("""INSERT INTO dom_fact_versions (fact_id, version, content, value_hash, schema_version, snapshot_ref,
                                                        revision_ref, derivation_key, changed_fields, rights_tags, observed_at, recorded_at)
                         VALUES (:f, :v, CAST(:c AS jsonb), :h, :sv, :snp, :exr, :dk, CAST(:cf AS jsonb), CAST(:r AS jsonb), :o, :rec)"""),
                 {"f": fact_id, "v": version + 1, "c": json.dumps(full, ensure_ascii=False), "h": digest,
                  "sv": schema_version, "dk": derivation_key, "snp": revision.snapshot_ref, "exr": revision.id, "cf": json.dumps(fields),
                  "r": json.dumps(rights_tags), "o": revision.observed_at, "rec": now})
    if not defer_closure:
        from . import requirements
        requirements.put(conn, full, source_closure if source_closure is not None else requirements.base(revision))
    data = {
        "info_domain": info_domain, "fact_id": fact_id, "changed_fields": fields,
        "rights_tags": rights_tags, "snapshot_ref": revision.snapshot_ref, "revision_ref": revision.id,
        "object_type": object_type, "fact_version": version + 1, "schema_version": schema_version}
    payloads.validator(FACT_UPSERTED, {}).validate(data)
    writer.append(conn, outbox.Event(type=FACT_UPSERTED, subject=fact_id, partition_key=fact_id, data=data))
    return full


def fact_version(conn: Connection, fact_id: str, version: int | None = None) -> dict | None:
    """内部读取事实的当前记录版本或固定版本；known_at选择与当前权利检查由application queries负责。"""
    if version is None:
        row = conn.execute(text("""SELECT v.content FROM dom_facts f JOIN dom_fact_versions v
                                   ON v.fact_id = f.id AND v.version = f.current_version WHERE f.id = :f"""), {"f": fact_id}).first()
    else:
        row = conn.execute(text("SELECT content FROM dom_fact_versions WHERE fact_id = :f AND version = :v"),
                           {"f": fact_id, "v": version}).first()
    return row[0] if row else None
