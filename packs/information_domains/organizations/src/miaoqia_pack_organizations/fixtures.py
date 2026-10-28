"""四条本地合成登记夹具；解析不生成 org_id 或生产组织身份。"""
from __future__ import annotations

import re
from pathlib import Path

from miaoqia.sdk import Document, ExtractionContext, ExtractionResult
from miaoqia.sdk.corpus_source import fixture_manifest

FIXTURE_MARKER = "合成测试企业登记夹具；非真实工商登记；不得用于生产组织核验"

MANIFEST = Path(__file__).resolve().parents[2] / "corpus/r2-fixture-registry-v1.json"


def load_registry_manifest(path: Path = MANIFEST) -> dict:
    value = fixture_manifest(path)
    if value["version"] != "0.1.0" or value["namespace"] != "fixture:r2-org":
        raise ValueError("组织夹具范围或版本不支持")
    for sample, source in value["samples"].items():
        if (not re.fullmatch(r"fixture-r2-org-[a-d]", sample) or source["source_id"] != "fixture-r2-org"
                or source["source_key"] != f"fixture:r2-org:{sample}"
                or source["rights_tag"] != f"fixture:r2-org-record-{sample[-1]}"):
            raise ValueError("组织夹具来源身份或标签不一致")
    data = value["data"]
    if set(data) != {"normalization", "records"} or data["normalization"] != "trim-only@0.1.0":
        raise ValueError("组织夹具规则不闭合")
    records = data["records"]
    if not isinstance(records, list) or len(records) != 4:
        raise ValueError("组织夹具必须有四个记录")
    keys, ids, samples = set(), set(), set()
    for record in records:
        if not isinstance(record, dict) or set(record) != {"sample", "fixture_subject_key", "org_id", "name", "aliases"}:
            raise ValueError("组织夹具记录字段不闭合")
        key, org, sample = record["fixture_subject_key"], record["org_id"], record["sample"]
        if not isinstance(key, str) or not re.fullmatch(r"fixture:r2-org:[a-d]", key):
            raise ValueError("组织夹具主体键无效")
        if not isinstance(org, str) or not re.fullmatch(r"org_[0-9A-HJKMNP-TV-Z]{26}", org):
            raise ValueError("组织夹具 ID 无效")
        if key in keys or org in ids or sample in samples or sample not in value["samples"] or key != f"fixture:r2-org:{sample[-1]}":
            raise ValueError("组织夹具主体键/ID/样本重复或未绑定")
        keys.add(key); ids.add(org); samples.add(sample)
        aliases = record["aliases"]
        if not isinstance(aliases, list) or len(aliases) != len(set(aliases)):
            raise ValueError("组织夹具别名无效")
        if any(not isinstance(n, str) or not n.startswith("【夹具】") or n != n.strip() for n in [record["name"], *aliases]):
            raise ValueError("组织夹具名称必须显式标记")
    if samples != set(value["samples"]):
        raise ValueError("组织夹具来源集合不一致")
    return value


class FixtureRegistrationExtractor:
    id = "org.fixture-registration"
    version = "0.1.0"
    fact_types = ("organization.registration",)

    def applies(self, doc: Document, source: dict) -> bool:
        return source.get("source_id") == "fixture-r2-org" and any(u.text == FIXTURE_MARKER for u in doc.units())

    def extract(self, doc: Document, ctx: ExtractionContext) -> ExtractionResult:
        # 标签精确且取值只 trim；通用 find_labeled 会裁句末标点，不适用身份键。
        values = {}
        for unit in doc.units():
            for label in ("夹具主体键", "名称", "曾用名称"):
                prefix = label + "："
                if unit.text.startswith(prefix):
                    tail = unit.text[len(prefix):]
                    start = len(prefix) + len(tail) - len(tail.lstrip())
                    end = len(unit.text.rstrip())
                    values.setdefault(label, []).append(unit.span(start, end))
        if len(values.get("夹具主体键", [])) != 1 or len(values.get("名称", [])) != 1:
            raise ValueError("组织夹具主体键和名称必须唯一")
        key, name = values["夹具主体键"][0], values["名称"][0]
        records = load_registry_manifest()["data"]["records"]
        record = next((r for r in records if r["fixture_subject_key"] == key.text), None)
        aliases = values.get("曾用名称", [])
        if record is None or name.text != record["name"] or [a.text for a in aliases] != record["aliases"]:
            raise ValueError("组织夹具原文与固定登记 manifest 不一致")
        return ExtractionResult(facts=[{"record_scope": "fixture",
            "subject": ctx.org(name, 1.0), "name": ctx.text(name, 1.0),
            "former_names": [ctx.text(alias, 1.0) for alias in aliases]}])
