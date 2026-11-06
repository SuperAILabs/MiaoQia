"""领域语义校验：由语料工具加载；不表示生产管线已接入。"""
from datetime import datetime

from miaoqia.sdk.document import Document
from miaoqia.sdk.validation import validate_field_quotes


def _literal_texts(node, path="facts"):
    """文本字段（只有 value 与 prov 的对象，契约 text 类型）是原文摘录：取值必须等于证据定位处的原文。"""
    if isinstance(node, dict):
        if set(node) == {"value", "prov"} and isinstance(node["value"], str) and isinstance(node["prov"], dict):
            yield path, node
        for key, value in node.items():
            yield from _literal_texts(value, f"{path}/{key}")
    elif isinstance(node, list):
        for i, value in enumerate(node):
            yield from _literal_texts(value, f"{path}/{i}")


def validate(doc: Document, facts: list[dict], evidence: dict[str, dict]) -> list[str]:
    fields = {key: (key + "_prov", None) for key in ("name", "phone", "email", "uscc")}
    fields["name_raw"] = ("prov", None)
    errors = validate_field_quotes(doc, facts, evidence, fields)
    for path, node in _literal_texts(facts):
        ref = evidence.get(node["prov"].get("evidence_ref"))
        if ref and doc.quote(ref["locator"]) != node["value"]:
            errors.append(f"{path}: 文本字段与证据原文不一致")
    for i, fact in enumerate(facts):
        categories = [c["category"] for c in fact.get("subject_categories", [])]
        codes = [(c["scheme"], c["scheme_version"], c["code"]) for c in categories]
        if len(codes) != len(set(codes)):
            errors.append(f"facts/{i}: 公告主题品类重复")
        lots = fact.get("lots", [])
        keys = [lot.get("lot_key") for lot in lots]
        if len(keys) != len(set(keys)):
            errors.append(f"facts/{i}: 标包号重复")
        for j, lot in enumerate(lots):
            result_ids = [x.get("id") for x in lot.get("bid_results", [])]
            if len(result_ids) != len(set(result_ids)):
                errors.append(f"facts/{i}/lots/{j}: 结果标识重复")
            for result in lot.get("bid_results", []):
                if fact.get("notice_type") in ("candidate", "award") and result["stage"] != fact["notice_type"]:
                    errors.append(f"facts/{i}/lots/{j}: 候选人与中标阶段不一致")
                if result["stage"] == "candidate" and result.get("is_winner"):
                    errors.append(f"facts/{i}/lots/{j}: 候选人不能当作已中标")
                if result.get("result_kind") == "framework_shortlist" and result.get("is_winner"):
                    errors.append(f"facts/{i}/lots/{j}: 框架入围不能当作第二阶段成交")
                members = [x.get("org_id") or x["name_raw"] for x in result.get("consortium_members", [])]
                if len(members) != len(set(members)):
                    errors.append(f"facts/{i}/lots/{j}: 联合体成员重复")
            if lot.get("ending") and any(x.get("is_winner") for x in lot.get("bid_results", [])):
                errors.append(f"facts/{i}/lots/{j}: 同一标包失败与中标互相矛盾")
        contract = fact.get("contract", {})
        if contract.get("term_start") and contract.get("term_end"):
            if datetime.fromisoformat(contract["term_start"]["value"].replace("Z", "+00:00")) > datetime.fromisoformat(contract["term_end"]["value"].replace("Z", "+00:00")):
                errors.append(f"facts/{i}/contract: 期限倒置")
    return errors


def validate_amendment(target, change, schema, registry, expected_version, actual_version):
    """采购更正禁止替换项目归属与公告关联；通用替换机制由SDK提供。"""
    from miaoqia.sdk.validation import validate_amendment as validate_replacement
    return validate_replacement(target, change, schema, registry, expected_version, actual_version,
                                immutable_fields=frozenset({"project_id", "related_notice_ids"}))
