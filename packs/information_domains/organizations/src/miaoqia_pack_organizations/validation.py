"""领域语义校验：由语料工具加载；不表示生产管线已接入。"""
from datetime import datetime

from miaoqia.sdk.document import Document
from miaoqia.sdk.validation import validate_field_quotes


def validate(doc: Document, facts: list[dict], evidence: dict[str, dict]) -> list[str]:
    fields = {key: (key + "_prov", None) for key in ("name", "phone", "email", "uscc")}
    fields["name_raw"] = ("prov", None)
    fields["legal_representative"] = ("legal_representative_prov", "name")
    errors = validate_field_quotes(doc, facts, evidence, fields)
    for i, fact in enumerate(facts):
        if fact.get("listed_on") and fact.get("removed_on"):
            if datetime.fromisoformat(fact["removed_on"]["value"].replace("Z", "+00:00")) < datetime.fromisoformat(fact["listed_on"]["value"].replace("Z", "+00:00")):
                errors.append(f"facts/{i}: 异常移出时间不能早于列入时间")
        parent, subject = fact.get("parent"), fact.get("subject")
        if parent and subject:
            if parent["name_raw"] == subject["name_raw"] or (parent.get("uscc") and parent["uscc"] == subject.get("uscc")):
                errors.append(f"facts/{i}: 分支机构不能以自身为隶属企业")
        if fact.get("change_type") == "name" and fact.get("before") and fact.get("after"):
            if fact["before"]["value"] == fact["after"]["value"]:
                errors.append(f"facts/{i}: 名称变更前后不能相同")
    return errors
