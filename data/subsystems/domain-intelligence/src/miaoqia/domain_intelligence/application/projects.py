"""冻结来源的保守采购项目聚合；公告原版本不回填，关联和依赖独立保存。"""
from __future__ import annotations

import hashlib
import json

from sqlalchemy import text

from miaoqia.contracts import models
from miaoqia.kernel import ids, outbox, payloads

from .facts import fact_version, write_version, writer

RULE = "procurement-project@0.1.0"
# 冻结官方语料的平台命名空间登记，不构成真实来源启用或跨平台编号互认。
NAMESPACES = {"official-qzygjy": "qzygjy:buyer-project-code"}
PROJECT_TYPE = "tendering.procurement_project"


def _emit(conn, name, notice, project_id, rights_tags, **extra):
    name = f"tendering.{name}.v1"
    data = {"project_id": project_id, "notice_id": notice["id"], "fact_version": notice["version"],
            "rights_tags": rights_tags, **extra}
    payloads.validator(name, {}).validate(data)
    writer.append(conn, outbox.Event(type=name, subject=notice["id"], partition_key=project_id or notice["id"], data=data))


def _identity(revision, notice):
    namespace = NAMESPACES.get(revision.source_id)
    code = notice.get("project_code", {})
    buyer = notice.get("buyer", {})
    if namespace is None:
        return None, "pending_namespace"
    if notice["notice_type"] not in ("tender", "award"):
        return None, "pending_notice_type"
    if not code.get("value", "").strip() or not code.get("prov"):
        return None, "pending_project_code"
    if not buyer.get("name_raw", "").strip() or not buyer.get("prov"):
        return None, "pending_buyer"
    code_value = code["value"].strip()  # 不删连字符、不改大小写、不猜年份
    if any(lot.get("project_code", {}).get("value", code_value).strip() != code_value
           for lot in notice.get("lots", [])):
        return None, "pending_conflicting_code"
    # name仅在该冻结平台内作受约束键，不宣称组织已解析。同名异体若提供USCC则分离。
    buyer_key = "uscc:" + buyer["uscc"] if buyer.get("uscc") else "name:" + buyer["name_raw"].strip()
    return (namespace, buyer_key, code_value), "confirmed"


def lock_identities(conn, *revisions):
    keys = set()
    for revision in revisions:
        for fact in revision.facts:
            identity, _ = _identity(revision, fact)
            if identity:
                keys.add(json.dumps(identity, ensure_ascii=False))
    for key in sorted(keys):
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "pp:" + key})


def aggregate_notice(conn, revision, notice):
    identity, status = _identity(revision, notice)
    if notice["version"] > 1:
        # Revision API 无来源修订序号；保留Notice版本，后续修订不得自动替换已确认聚合依据。
        identity, status = None, "pending_revision"
    project_id, project = None, None
    if identity:
        namespace, buyer_key, code = identity
        lock = json.dumps(identity, ensure_ascii=False)
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "pp:" + lock})
        project_id = conn.execute(text("""SELECT project_id FROM dom_project_keys
            WHERE namespace=:n AND buyer_key=:b AND project_code=:c"""),
            {"n": namespace, "b": buyer_key, "c": code}).scalar_one_or_none()
        if project_id is None:
            project_id = ids.new("pp")
            conn.execute(text("""INSERT INTO dom_project_keys VALUES (:n,:b,:c,:p,:r)"""),
                         {"n": namespace, "b": buyer_key, "c": code, "p": project_id, "r": RULE})
        prev = fact_version(conn, project_id)
        members = []
        if prev:
            members = conn.execute(text("""SELECT v.content FROM dom_project_dependencies d
                JOIN dom_fact_versions v ON v.fact_id=d.notice_id AND v.version=d.notice_version
                WHERE d.project_id=:p AND d.project_version=:v"""),
                {"p": project_id, "v": prev["version"]}).scalars().all()
        project = _write_project(conn, revision, notice, project_id, members, prev, namespace, lock)
        if project and (not prev or prev["stage"] != project["stage"]):
            _emit(conn, "project.stage_changed", notice, project_id, project["rights_tags"],
                  **{"from": prev["stage"] if prev else None, "to": project["stage"]})
    reason = {"rule": RULE, "source_id": revision.source_id, "identity": identity,
              "project_code": notice.get("project_code"), "buyer": notice.get("buyer"),
              "project_version": project["version"] if project else None,
              "lots": "unconfirmed: no cross-notice lot evidence"}
    conn.execute(text("INSERT INTO dom_notice_links VALUES (:n,:v,:p,:s,CAST(:r AS jsonb))"),
                 {"n": notice["id"], "v": notice["version"], "p": project_id, "s": status,
                  "r": json.dumps(reason, ensure_ascii=False)})
    if notice["version"] == 1:
        event_rights = project["rights_tags"] if project else notice["rights_tags"]
        _emit(conn, "notice.published", notice, project_id, event_rights, notice_type=notice["notice_type"])
        if project_id:
            _emit(conn, "project.linked", notice, project_id, project["rights_tags"],
                  method="project_code", confidence=1, status="confirmed")
        if notice["notice_type"] == "award":
            _emit(conn, "result.published", notice, project_id, event_rights, stage="award", lot_ids=[])


def _write_project(conn, revision, notice, project_id, members, prev, source_id, source_key):
    """唯一采购投影写入原语；refs是同次写入deps的公开投影。"""
    members = sorted({m["id"]: m for m in [*members, notice]}.values(), key=lambda n: (n.get("published_at", {}).get("value", ""), n["id"]))
    # 优先招标标题；相同语义输入选择稳定，不能按到达顺序退回阶段。
    selected = min(members, key=lambda n: (n["notice_type"] != "tender", n["id"]))
    stage = "awarded" if any(n["notice_type"] == "award" for n in members) else "announced"
    body = {"stage": stage, "title": selected["title"], "buyer": selected["buyer"],
            "project_code": selected["project_code"], "notice_ids": [n["id"] for n in members],
            "notice_refs": [{"id": n["id"], "version": n["version"]} for n in members], "lots": []}
    rights = sorted({tag for n in members for tag in n["rights_tags"]})
    derivation = hashlib.sha256(json.dumps([RULE, [(n["id"], n["version"]) for n in members]],
                                          sort_keys=True).encode()).hexdigest()
    project = write_version(conn, revision, body, fact_id=project_id, version=prev["version"] if prev else 0,
                            prev=prev, object_type=PROJECT_TYPE, model=models.ProcurementProject,
                            schema_version="1.3.0", source_id=source_id, source_key=source_key,
                            derivation_key=derivation, rights_tags=rights, compare_evidence=True)
    if project:
        for member in members:
            conn.execute(text("INSERT INTO dom_project_dependencies VALUES (:p,:v,:n,:nv)"),
                         {"p": project_id, "v": project["version"], "n": member["id"], "nv": member["version"]})
    return project


def refresh_enriched_member(conn, revision, notice, receipt):
    """只从本事务已持久化receipt读取source-base；不接受请求传入trusted布尔值。"""
    proof = conn.execute(text("SELECT * FROM dom_enrichment_receipts WHERE run_key=:r AND notice_id=:n AND output_version=:v"),
                         {"r": receipt, "n": notice["id"], "v": notice["version"]}).mappings().one()
    base = fact_version(conn, notice["id"], proof["source_base_version"])
    root_link = conn.execute(text("SELECT * FROM dom_notice_links WHERE notice_id=:n AND notice_version=:v"),
                             {"n": notice["id"], "v": base["version"]}).mappings().one_or_none()
    identity_fields = lambda n: (n["notice_type"], n.get("project_code", {}).get("value"),
                                n.get("buyer", {}).get("name_raw"), n.get("buyer", {}).get("uscc"))
    if identity_fields(base) != identity_fields(notice) or base["revision_ref"] != notice["revision_ref"]:
        raise ValueError("增强不得改变来源身份")
    project_id, project = None, None
    if root_link and root_link["status"] == "confirmed" and root_link["project_id"]:
        project_id = root_link["project_id"]
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"), {"k": "pp-id:" + project_id})
        prev = fact_version(conn, project_id)
        # 正常聚合和刷新还需共用同一个身份锁，调用方已先持有source→identity锁。
        row = conn.execute(text("SELECT source_id,source_key FROM dom_facts WHERE id=:p"), {"p": project_id}).one()
        members = conn.execute(text("""SELECT v.content FROM dom_project_dependencies d JOIN dom_fact_versions v
            ON v.fact_id=d.notice_id AND v.version=d.notice_version WHERE d.project_id=:p AND d.project_version=:v"""),
            {"p": project_id, "v": prev["version"]}).scalars().all()
        if notice["id"] not in {m["id"] for m in members}:
            raise ValueError("source-base不是已接受项目成员")
        project = _write_project(conn, revision, notice, project_id, members, prev, row.source_id, row.source_key)
    reason = {"rule": RULE, "enrichment_receipt": receipt, "source_base_version": base["version"],
              "project_version": project["version"] if project else None}
    conn.execute(text("INSERT INTO dom_notice_links VALUES (:n,:v,:p,:s,CAST(:r AS jsonb))"),
                 {"n": notice["id"], "v": notice["version"], "p": project_id,
                  "s": "confirmed" if project_id else "pending_revision", "r": json.dumps(reason)})
