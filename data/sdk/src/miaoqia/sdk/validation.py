"""通用文档与证据完整性验证、受约束的字段替换验证；Schema仍负责结构/类型。"""
from __future__ import annotations

import hashlib
from collections.abc import Callable

from .document import Document


def validate_extraction(doc: Document, facts: list[dict], evidence: dict[str, dict], *,
                        semantic_validator: Callable | None = None) -> list[str]:
    """返回可复核错误；不修正源数据，不把无效证据静默变为有效。

    领域校验（semantic_validator）只在通用文档与证据校验全部通过后执行，因为领域规则依赖有效证据；
    因此证据有错时只报告通用错误，修复后再次运行才会出现领域错误。
    """
    errors = []

    def require(condition):
        if not condition:
            raise ValueError("语义约束失败")

    for i, block in enumerate(doc.blocks):
        if block.get("index") != i:
            errors.append(f"blocks/{i}: 块序号必须连续且与位置一致")
        if block.get("kind") == "table":
            rows = block.get("rows", [])
            if rows and any(len(row) != len(rows[0]) for row in rows):
                errors.append(f"blocks/{i}: 表格必须为完整矩形网格")
            for ri, row in enumerate(rows):
                for ci, cell in enumerate(row):
                    target = cell.get("covered_by")
                    if target:
                        try:
                            tr, tc = target["row"], target["col"]
                            require(0 <= tr <= ri and 0 <= tc < len(rows[tr]))
                            require(not rows[tr][tc].get("covered_by") and cell["text"] == "")
                            origin = rows[tr][tc]
                            require(tr <= ri < tr + origin.get("row_span", 1))
                            require(tc <= ci < tc + origin.get("col_span", 1))
                        except (KeyError, IndexError, TypeError, ValueError, AssertionError):
                            errors.append(f"blocks/{i}/rows/{ri}/{ci}: 合并单元格指针无效")

    valid_refs = set()
    for key, ref in evidence.items():
        try:
            require(ref["id"] == key and key.startswith("ev_"))
            require(ref["snapshot_ref"] == doc.snapshot_ref)
            require(ref["pipeline_version"] == doc.pipeline_version)
            loc = ref["locator"]
            first, last = loc["block"], loc.get("block_end", loc["block"])
            require(0 <= first <= last < len(doc.blocks))
            require(("row" in loc) == ("col" in loc))
            if "row" in loc:
                require(first == last and doc.blocks[first]["kind"] == "table")
                require(0 <= loc["row"] < len(doc.blocks[first]["rows"]))
                require(0 <= loc["col"] < len(doc.blocks[first]["rows"][loc["row"]]))
                cell = doc.blocks[first]["rows"][loc["row"]][loc["col"]]
                require(not cell.get("covered_by"))
                text = cell["text"]
                require(0 <= loc.get("char_start", 0) <= loc.get("char_end", len(text)) <= len(text))
            else:
                require(all(doc.blocks[b]["kind"] != "table" for b in range(first, last + 1)))
                head, tail = doc.blocks[first].get("text", ""), doc.blocks[last].get("text", "")
                require(0 <= loc.get("char_start", 0) <= len(head))
                require(0 <= loc.get("char_end", len(tail)) <= len(tail))
                if first == last:
                    require(loc.get("char_start", 0) <= loc.get("char_end", len(tail)))
            quote = doc.quote(loc)
            require(quote and ref["quote_hash"] == "sha256:" + hashlib.sha256(quote.encode()).hexdigest())
            valid_refs.add(key)
        except (KeyError, IndexError, TypeError, ValueError, AssertionError):
            errors.append(f"evidence/{key}: 证据归属、定位或哈希不符")

    for path, node in _nodes(facts):
        if "evidence_ref" in node and node["evidence_ref"] not in valid_refs:
            errors.append(f"{path}: 证据引用不存在或无效")
    if not errors and semantic_validator is not None:
        result = semantic_validator(doc, facts, evidence)
        if not isinstance(result, list) or any(not isinstance(error, str) for error in result):
            raise ValueError("领域校验器必须返回错误字符串列表，不能以None或其他类型表示通过")
        return result
    return errors


def validate_amendment(target: dict, change: dict, schema: dict, registry,
                       expected_version: int, actual_version: int, *,
                       immutable_fields: frozenset[str] = frozenset()) -> list[str]:
    """验证已有字段的替换：不执行写入；新增/删除/未知路径必须另经明确规则。"""
    import re
    from urllib.parse import urldefrag, urljoin

    from jsonschema import Draft202012Validator

    if expected_version != actual_version:
        return ["AMENDMENT_STALE_VERSION"]
    pointer = change.get("field_path", "")
    if not pointer.startswith("/") or re.search(r"~(?![01])", pointer):
        return ["AMENDMENT_INVALID_POINTER"]
    parts = [p.replace("~1", "/").replace("~0", "~") for p in pointer[1:].split("/")]
    immutable = {"id", "info_domain", "schema_version", "version", "snapshot_ref", "revision_ref",
                 "rights_tags", "classification", "observed_at", "recorded_at"} | immutable_fields
    if parts[0] in immutable or parts[-1] == "id":
        return ["AMENDMENT_IMMUTABLE_FIELD"]
    current, node, base = target, schema, schema["$id"]
    try:
        for part in parts:
            while "$ref" in node:
                uri = urljoin(base, node["$ref"])
                resolved = registry.resolver(base).lookup(uri)
                node = resolved.contents
                base = urldefrag(uri)[0]
            if isinstance(current, list):
                if not re.fullmatch(r"0|[1-9][0-9]*", part):
                    return ["AMENDMENT_INVALID_POINTER"]
                current = current[int(part)]
                node = node["items"]
            else:
                current = current[part]
                node = node["properties"][part]
        # 保留字段Schema的原始base，不能把相对$ref解析到调用者根目录。
        validator = Draft202012Validator({"$id": base, **node}, registry=registry,
                                        format_checker=Draft202012Validator.FORMAT_CHECKER)
        if "before" not in change or "after" not in change:
            return ["AMENDMENT_INCOMPLETE_REPLACEMENT"]
        if change["before"] != current:
            # provenance可来自更正公告：值比较剔除证据元数据，但不忽略业务字段。
            def without_prov(value):
                if isinstance(value, dict):
                    return {k: without_prov(v) for k, v in value.items() if k not in ("prov", "raw")}
                if isinstance(value, list):
                    return [without_prov(v) for v in value]
                return value
            if without_prov(change["before"]) != without_prov(current):
                return ["AMENDMENT_BEFORE_MISMATCH"]
        if list(validator.iter_errors(change["before"])) or list(validator.iter_errors(change["after"])):
            return ["AMENDMENT_VALUE_SCHEMA_MISMATCH"]
    except (KeyError, IndexError, TypeError, ValueError):
        return ["AMENDMENT_UNKNOWN_FIELD"]
    return []


def validate_field_quotes(doc: Document, facts: list[dict], evidence: dict[str, dict],
                          fields: dict[str, tuple[str, str | None]]) -> list[str]:
    """按调用方声明校验字面字段；字段语义由领域包提供，证据完整性须先通过。"""
    errors = []
    for path, node in _nodes(facts):
        for field, (prov_key, value_key) in fields.items():
            value = node.get(field)
            proof = node.get(prov_key, {})
            ref = evidence.get(proof.get("evidence_ref")) if isinstance(proof, dict) else None
            expected = value.get(value_key) if isinstance(value, dict) and value_key else value
            if expected is not None and ref and doc.quote(ref["locator"]) != expected:
                errors.append(f"{path}/{field}: 字段与证据不一致")
    return errors


def _nodes(node, path="facts"):
    """通用树遍历，不解释领域字段。"""
    if isinstance(node, dict):
        yield path, node
        for key, value in node.items():
            yield from _nodes(value, f"{path}/{key}")
    elif isinstance(node, list):
        for i, value in enumerate(node):
            yield from _nodes(value, f"{path}/{i}")
