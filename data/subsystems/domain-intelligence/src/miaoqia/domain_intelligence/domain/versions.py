"""领域事实版本的纯规则：取值比较、变化字段与部分标识的跨版本稳定。"""
from __future__ import annotations

import hashlib
import json

META = frozenset({"id", "schema_version", "version", "info_domain", "snapshot_ref", "revision_ref", "rights_tags",
                  "classification", "observed_at", "recorded_at"})


def _values(node):
    """去掉字段溯源（prov 与 *_prov）与部分标识（id）后的取值；数组保持顺序。"""
    if isinstance(node, dict):
        return {k: _values(v) for k, v in node.items() if k != "prov" and not k.endswith("_prov") and k != "id"}
    if isinstance(node, list):
        return [_values(v) for v in node]
    return node


def value_hash(fact: dict) -> str:
    body = {k: _values(v) for k, v in fact.items() if k not in META}
    return "sha256:" + hashlib.sha256(json.dumps(body, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def _escape(key: str) -> str:
    return key.replace("~", "~0").replace("/", "~1")


def changed_fields(prev: dict | None, new: dict) -> list[str]:
    """顶层领域字段的变化（JSON Pointer）；首个版本为全部领域字段。"""
    keys = sorted(k for k in set(new) | set(prev or {}) if k not in META)
    if prev is None:
        return [f"/{_escape(k)}" for k in keys if k in new]
    return [f"/{_escape(k)}" for k in keys if _values(prev.get(k)) != _values(new.get(k))]


def stabilize_result_ids(prev: dict | None, new: dict) -> dict:
    """同一标包、同一阶段、同一投标人原文名称的结果沿用上一版本的 bdr_ 标识（信号的去重键依赖它，见 detection api.Unit.Key）。

    新快照的解析器按快照分配标识；不沿用会让同一条中标结果在更正或重新抓取后被当成新的求值单位。
    """
    if prev is None:
        return new
    known: dict[tuple[str, str, str], str] = {}
    for lot in prev.get("lots", []):
        for r in lot.get("bid_results", []):
            known.setdefault((lot["lot_key"], r["stage"], r["bidder"]["name_raw"]), r["id"])
    used: set[str] = set()
    for lot in new.get("lots", []):
        for r in lot.get("bid_results", []):
            key = (lot["lot_key"], r["stage"], r["bidder"]["name_raw"])
            if key in known and known[key] not in used:
                r["id"] = known[key]
            used.add(r["id"])
    return new
