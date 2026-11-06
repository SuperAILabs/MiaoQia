"""领域语义校验：由语料工具与已登记的文档管线加载；不表示真实来源已启用。"""
from datetime import datetime

from miaoqia.sdk.document import Document
from .fixtures import FIXTURE_MARKER
from miaoqia.sdk.validation import validate_field_quotes


def validate(doc: Document, facts: list[dict], evidence: dict[str, dict]) -> list[str]:
    fields = {key: (key + "_prov", None) for key in ("name", "phone", "email", "uscc")}
    fields["name_raw"] = ("prov", None)
    fields["legal_representative"] = ("legal_representative_prov", "name")
    errors = validate_field_quotes(doc, facts, evidence, fields)
    fixture = any(u.text == FIXTURE_MARKER for u in doc.units())
    for i, fact in enumerate(facts):
        if fixture != (fact.get("record_scope") == "fixture"):
            errors.append(f"facts/{i}: 合成登记夹具必须显式携带 record_scope，真实记录不能伪造夹具标记")
        if fixture:
            if set(fact) - {"fact_type", "record_scope", "subject", "name", "former_names"}:
                errors.append(f"facts/{i}: 合成登记夹具不得断言真实登记属性")
            if any(not x.startswith("【夹具】") for x in [fact.get("subject", {}).get("name_raw", ""), fact.get("name", {}).get("value", ""), *[a.get("value", "") for a in fact.get("former_names", [])]]):
                errors.append(f"facts/{i}: 合成登记名称必须带夹具标记")
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
