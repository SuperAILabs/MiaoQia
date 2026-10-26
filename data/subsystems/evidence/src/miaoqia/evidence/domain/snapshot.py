"""快照判定：同一来源对象的规范化内容哈希不变即同一快照（不含快照标识本身）。"""
from __future__ import annotations

import hashlib
import json


def normalized_hash(document: dict) -> str:
    body = {k: v for k, v in document.items() if k != "snapshot_ref"}
    canonical = json.dumps(body, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return "sha256:" + hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def raw_hash(raw: bytes) -> str:
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def quality_status(errors: list[str], warnings: list[dict], facts: list[dict]) -> str:
    """按最严格状态汇总：证据或领域语义校验不通过、或任一事实被解析器标为 suspect，修订即为 suspect（进入复核，
    不物化为领域事实）；解析警告或事实质量警告为 warning。"""
    statuses = {f.get("quality", {}).get("status") for f in facts}
    if errors or "suspect" in statuses:
        return "suspect"
    if warnings or "warning" in statuses:
        return "warning"
    return "ok"
