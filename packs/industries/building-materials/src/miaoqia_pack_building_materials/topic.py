"""固定YAML的保守字面主题规则；仅复核官方pair允许发布，不代表供应商实际供货。"""
from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path

import yaml

from miaoqia.evidence.api import Recipe
from miaoqia.sdk.derivation import fingerprint

RECIPE = "proc.building-topic@0.1.0"
REFERENCE = {"name": "proc-building-topic", "version": "0.1.0"}
TRANSFORM = "building.literal-topic@0.1.0"
# 版本化控制词：不能用单字非/不拒绝非政府采购或不锈钢。
VETO = ["不采购", "不含", "不包括", "不在", "除外", "以外", "取消", "终止", "仅供参考", "或", "二选一", "可选", "替代"]


def match(title, content):
    """返回全部最大字面跨度；unknown用空候选表达，不补齐省略产品。"""
    products = content["vocabulary"]["vocabulary"]["products"]
    if any(t in title for t in content["config"]["veto"]):
        return []
    hits = []
    for product in products:
        for term in [product["term"], *product.get("synonyms", [])]:
            start = 0
            while (start := title.find(term, start)) >= 0:
                if title[:start].endswith(("非", "不是")):
                    return []
                hits.append((start, start + len(term), product))
                start += 1
    # 只有分隔两边均含独立完整产品词时才局部not_when。
    parts = re.split("[与和及、]", title)
    complete = len(parts) > 1 and all(any(p["term"] in part or any(t in part for t in p.get("synonyms", [])) for p in products) for part in parts)
    if not complete and re.search(r"(?:PP|PE|PVC|HDPE)[与和及、]", title):
        return []
    accepted = []
    for start, end, product in hits:
        context = title
        if complete:
            left = max([title.rfind(c, 0, start) for c in "与和及、"]) + 1
            right = min([i for c in "与和及、" if (i := title.find(c, end)) >= 0] or [len(title)])
            context = title[left:right]
        if not any(t in context for t in product.get("not_when", [])):
            accepted.append((start, end, product["category"]))
    maximal = [h for h in accepted if not any(a <= h[0] and b >= h[1] and (a, b) != h[:2] for a, b, _ in accepted)]
    for i, (a, b, code) in enumerate(maximal):
        if any(max(a, c) < min(b, d) and (a, b, code) != (c, d, other) for c, d, other in maximal[i+1:]):
            return []
    result = {}
    for a, b, code in sorted(maximal):
        result.setdefault(code, {"code": code, "span": [a, b], "literal_match": True})
    return list(result.values())


def transform(inputs, references):
    if len(inputs) != 1 or len(references) != 1 or not isinstance(inputs[0], str):
        raise ValueError("主题转换输入错误")
    return match(inputs[0], references[0])


def build_recipe(packs_dir):
    root = Path(packs_dir)
    industry = root / "industries/building-materials"
    procurement = root / "information_domains/procurement"
    blobs = {name: (industry / name).read_bytes() for name in ("categories.yaml", "vocabulary.yaml", "topic-evaluation.json")}
    expected = {"categories.yaml": "9996ac4b98ae2747171abbd8084f55f11961eae31b9e03409db92cc69b65e5df", "vocabulary.yaml": "7bc0e053f791f352a5a6eb5a4b2d4f102f6daf011a5eca278852d71e5a45192a"}
    if any(hashlib.sha256(blobs[name]).hexdigest() != digest for name, digest in expected.items()):
        raise ValueError("固定YAML版本内容改变")
    scope = json.loads((procurement / "corpus/building-topic-scope.json").read_text())
    for review in scope["reviews"]:
        if hashlib.sha256((procurement / review["review_ref"]).read_bytes()).hexdigest() != review["review_sha256"]:
            raise ValueError("scope复核原件引用改变")
    if hashlib.sha256((procurement / scope["lifecycle_ref"]).read_bytes()).hexdigest() != scope["lifecycle_sha256"]:
        raise ValueError("scope生命周期引用改变")
    content = {"taxonomy": yaml.safe_load(blobs["categories.yaml"]), "vocabulary": yaml.safe_load(blobs["vocabulary.yaml"]),
               "config": {"version": "0.1.0", "veto": VETO, "min_input_confidence": 0.8, "scoring": "binary-all-hard-gates", "overlap": "maximal-literal", "scope": "reviewed-pair-only"}, "scope": scope,
               "hashes": {n: hashlib.sha256(v).hexdigest() for n, v in blobs.items()}}
    if fingerprint(content) != EXPECTED_REFERENCE:
        raise ValueError("固定recipe参考内容改变；必须升版本")

    def outputs(source, candidates, ref, snapshot):
        review = next((r for r in scope["reviews"] if all(snapshot.get(k) == r[k] for k in ("source_id", "source_key", "raw_sha256"))
                       and source["title"]["value"] == r["title"] and source["notice_type"] == r["notice_type"]), None)
        eligible = review is not None
        confidence = min(1, source["title"]["prov"]["confidence"])
        published = candidates if eligible and confidence >= 0.8 else []
        taxonomy = content["taxonomy"]["taxonomy"]
        categories = [{"category": {"scheme": taxonomy["scheme"], "scheme_version": taxonomy["scheme_version"], "code": c["code"], "confidence": confidence},
                       "scope": "notice_subject", "source_field": "/title/value",
                       "prov": {**source["title"]["prov"], "evidence_ref": ref["id"], "method": "rule", "extractor": TRANSFORM, "confidence": confidence}} for c in published]
        guard = {"eligible": eligible, "source_hash": fingerprint(source), "snapshot_ref": source["snapshot_ref"],
                 "raw_sha256": snapshot["raw_sha256"], "review_ref": review["review_ref"] if review else None,
                 "review_sha256": review["review_sha256"] if review else None, "reference_hash": fingerprint(content),
                 "title_evidence_ref": source["title"]["prov"]["evidence_ref"], "title_spans": [c["span"] for c in candidates]}
        return [{"path": "/subject_categories", "value": categories, "evidence_ref": ref["id"], "scope_check": guard,
                 "diagnostics": {"candidates": candidates, "rule_score": 1 if categories else 0, "published": bool(categories)}}]
    return Recipe(RECIPE, "/title/value", TRANSFORM, REFERENCE, content, transform, outputs)


EXPECTED_REFERENCE = "sha256:2b5aef8fb787639360a27918f62ee56c4fb1c573f0c564ed288e1660fd13e258"
