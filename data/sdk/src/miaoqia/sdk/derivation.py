"""固定输入的确定性派生验证；存储和权限由所有者提供，不执行网络读取。"""
from __future__ import annotations

import hashlib
import re

import rfc8785
from miaoqia.contracts import models

from .document import Document
from .validation import validate_extraction


def canonical(value):
    return rfc8785.dumps(value)


def fingerprint(value):
    return "sha256:" + hashlib.sha256(canonical(value)).hexdigest()


def pointer(value, path):
    if not isinstance(path, str) or not path.startswith("/") or re.search(r"~(?![01])", path):
        raise ValueError("无效固定字段路径")
    node = value
    for raw in path[1:].split("/"):
        part = raw.replace("~1", "/").replace("~0", "~")
        if isinstance(node, list):
            if not re.fullmatch(r"0|[1-9][0-9]*", part):
                raise ValueError("无效数组路径")
            node = node[int(part)]
        else:
            node = node[part]
    return node


def field_input(fact, path):
    """支持既有标量字段或带prov的值容器，字段必须确有自身/父容器证据。"""
    value = pointer(fact, path)
    parent = pointer(fact, path.rsplit("/", 1)[0]) if "/" in path[1:] else fact
    proof = value.get("prov") if isinstance(value, dict) else None
    proof = proof or (parent.get("prov") if isinstance(parent, dict) else None)
    if not proof or not proof.get("evidence_ref"):
        raise ValueError("固定字段没有匹配的证据")
    return value, proof["evidence_ref"]


def evaluate(root, *, get_ref, get_document, get_fact, get_reference, transforms):
    """验证完整无环链并重算值。record首批明确拒绝；每次验证而非信任调用方输出。"""
    active, cache, heights = set(), {}, {}

    def visit(ref_id):
        if ref_id in active:
            raise ValueError("派生证据存在环")
        if ref_id in cache:
            if len(active) + heights[ref_id] > 64:
                raise ValueError("派生证据图超界")
            return cache[ref_id]
        if len(active) >= 64 or len(active) + len(cache) >= 1024:
            raise ValueError("派生证据图超界")
        active.add(ref_id)
        ref = get_ref(ref_id)
        if not ref or ref.get("id") != ref_id:
            raise ValueError("派生输入证据不存在")
        height = 1
        kind = ref.get("kind", "doc")
        if kind == "doc":
            doc = Document(get_document(ref["snapshot_ref"]))
            plain = {k: v for k, v in ref.items() if k != "kind"}
            if validate_extraction(doc, [], {ref_id: plain}):
                raise ValueError("文档叶子完整性失败")
            result = doc.quote(ref["locator"])
        elif kind == "derivation":
            models.DerivationEvidenceRef.model_validate(ref)
            transform = transforms.get(ref["transform"])
            if transform is None:
                raise ValueError("转换未登记或未启用")
            inputs = []
            for edge in ref["inputs"]:
                child = visit(edge["evidence_ref"])
                height = max(height, 1 + heights[edge["evidence_ref"]])
                if "fact_ref" in edge:
                    key = edge["fact_ref"]
                    fact = get_fact(key["id"], key["version"])
                    if not fact or fact.get("id") != key["id"] or fact.get("version") != key["version"]:
                        raise ValueError("固定事实版本不存在")
                    value, evidence_id = field_input(fact, edge["field_path"])
                    if evidence_id != edge["evidence_ref"]:
                        raise ValueError("字段路径与证据不匹配")
                    if get_ref(evidence_id).get("kind", "doc") == "derivation" and value != child:
                        raise ValueError("字段与派生输入值不匹配")
                    inputs.append(value)
                else:
                    if "field_path" in edge:
                        raise ValueError("字段路径缺少固定事实版本")
                    inputs.append(child)
            references = []
            for key in ref.get("reference_data", []):
                record = get_reference(key["name"], key["version"])
                if not record or fingerprint(record["content"]) != record["hash"]:
                    raise ValueError("不可变参考数据缺失或哈希不符")
                references.append(record["content"])
            result = transform(inputs, references)
            if fingerprint(result) != ref["value_hash"]:
                raise ValueError("派生输出哈希/重算不符")
        else:
            raise ValueError("不支持的证据kind（record尚未实现）")
        active.remove(ref_id)
        heights[ref_id] = height
        cache[ref_id] = result
        return result

    return visit(root)
