"""已验证evidence结果的受控Notice增强；CAS及pp刷新与原writer同事务。"""
from __future__ import annotations

import copy
import json
import re

from sqlalchemy import text

from miaoqia.contracts import models
from miaoqia.sdk.derivation import fingerprint, pointer

from ..domain.versions import META
from .facts import fact_version, write_version
from .projects import lock_identities, refresh_enriched_member
from .source import validate_source


class StaleEnrichment(ValueError):
    pass


def _row(conn, notice_id):
    row = conn.execute(text("SELECT * FROM dom_facts WHERE id=:n"), {"n": notice_id}).first()
    if row is None or row.info_domain != "INF-PROC" or row.object_type != "tendering.notice":
        raise ValueError("目标不是已物化INF-PROC公告")
    return row


def _source_base(conn, current):
    row = conn.execute(text("""SELECT source_base_version FROM dom_enrichment_receipts
        WHERE notice_id=:n AND output_version=:v AND parent_version<output_version LIMIT 1"""),
        {"n": current["id"], "v": current["version"]}).first()
    return fact_version(conn, current["id"], row[0]) if row else current


def prepare(database, extractions, notice_ids):
    if not 1 <= len(notice_ids) <= 100 or any(not re.fullmatch(r"ntc_[0-9A-HJKMNP-TV-Z]{26}", i) for i in notice_ids):
        raise ValueError("必须显式指定1–100个ntc_标识")
    results = []
    with database.tx() as conn:
        for notice_id in sorted(set(notice_ids)):
            row = _row(conn, notice_id)
            current = fact_version(conn, notice_id)
            source = _source_base(conn, current)
            validate_source(source, row, extractions.revision(source["revision_ref"]))
            results.append({"source": source, "expected_parent": current["version"]})
    return results


def apply(database, extractions, enrichments, run_key, *, expected_parent):
    if type(expected_parent) is not int or expected_parent < 1:
        raise ValueError("expected_parent必须为正整数")
    # 先跨包读取并重验完整evidence结果；domain持锁后不跨DB写入。
    result = enrichments.get(run_key)
    if result is None:
        raise PermissionError("增强结果不存在或当前权利拒绝")
    topic = result["recipe"] == "proc.building-topic@0.1.0" and result.get("test_recipe") is False
    if not topic and not result.get("test_recipe"):
        raise ValueError("未启用的封闭recipe")
    source_id = result["source"]["id"]
    revision = extractions.revision(result["revision_ref"])
    with database.tx() as conn:
        row = _row(conn, source_id)
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtext(:k))"),
                     {"k": f"{row.info_domain}|{row.source_id}|{row.source_key}"})
        receipt = conn.execute(text("SELECT output_version FROM dom_enrichment_receipts WHERE run_key=:k"), {"k": run_key}).first()
        if receipt:
            return {"notice_id": source_id, "version": receipt[0], "action": "unchanged", "run_key": run_key}
        current = fact_version(conn, source_id)
        source = _source_base(conn, current)
        if current["version"] != expected_parent or source["version"] != result["source"]["version"]:
            raise StaleEnrichment("增强parent/source-base已过期")
        validate_source(source, row, revision)
        if fingerprint(source) != result["source_hash"]:
            raise ValueError("增强source-base指纹不符")
        enrichments.require_full(current["rights_tags"])
        fact = {k: copy.deepcopy(v) for k, v in current.items() if k not in META}
        for output in result["outputs"]:
            path = output["path"]
            if topic:
                if (len(result["outputs"]) != 1 or set(output) != {"path", "value", "evidence_ref", "scope_check", "diagnostics"}
                        or path != "/subject_categories" or not isinstance(output["value"], list)):
                    raise ValueError("主题增强必须替换其拥有的整个字段")
                guard = output["scope_check"]
                if guard["source_hash"] != result["source_hash"] or guard["reference_hash"] != result["reference_hash"] or guard["snapshot_ref"] != source["snapshot_ref"]:
                    raise ValueError("主题范围证明与固定source不符")
                codes = []
                for item in output["value"]:
                    category, prov = item["category"], item["prov"]
                    if (not guard["eligible"] or item["scope"] != "notice_subject" or item["source_field"] != "/title/value"
                            or prov["evidence_ref"] != output["evidence_ref"] or prov["confidence"] != category["confidence"]
                            or not 0.8 <= prov["confidence"] <= source["title"]["prov"]["confidence"]):
                        raise ValueError("主题证据或发布门槛不符")
                    codes.append((category["scheme"], category["scheme_version"], category["code"]))
                if len(codes) != len(set(codes)):
                    raise ValueError("主题品类重复")
                fact.pop("subject_categories", None)
                if output["value"]:
                    fact["subject_categories"] = copy.deepcopy(output["value"])
                continue
            if not re.fullmatch(r"/(buyer|lots/(0|[1-9][0-9]*)/bid_results/(0|[1-9][0-9]*)/bidder)/prov", path):
                raise ValueError("增强字段不在c0允许列表")
            if output["value"].get("evidence_ref") != output["evidence_ref"]:
                raise ValueError("派生证据与补丁不符")
            parent = pointer(fact, path.rsplit("/", 1)[0])
            parent["prov"] = copy.deepcopy(output["value"])
        lock_identities(conn, revision)
        full = write_version(conn, revision, fact, fact_id=source_id, version=current["version"], prev=current,
                             object_type="tendering.notice", model=models.Notice, schema_version="1.2.0" if topic else current["schema_version"],
                             source_id=row.source_id, source_key=row.source_key, ordinal=row.ordinal,
                             derivation_key="enhance:" + run_key, rights_tags=sorted(set(current["rights_tags"]) | set(result["rights_tags"])), compare_evidence=True)
        output_version = full["version"] if full else current["version"]
        conn.execute(text("""INSERT INTO dom_enrichment_receipts VALUES
            (:k,:n,:s,:p,:v,:r,:sh,:ih,:oh,:rh,CAST(:e AS jsonb),CAST(:o AS jsonb),now())"""),
            {"k": run_key, "n": source_id, "s": source["version"], "p": current["version"], "v": output_version,
             "r": result["recipe"], "sh": result["source_hash"], "ih": result["input_hash"],
             "oh": fingerprint({"fact": full or current, "outputs": result["outputs"]}), "rh": result["reference_hash"],
             "e": json.dumps([o["evidence_ref"] for o in result["outputs"]]), "o": json.dumps(result["outputs"])})
        if full:
            refresh_enriched_member(conn, revision, full, run_key)
        return {"notice_id": source_id, "version": output_version, "action": "applied" if full else "recorded",
                "run_key": run_key}
