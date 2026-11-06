"""冻结语料的来源描述；合成夹具是本地制品，不能声明公开抓取来源或权限许可。"""
from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass
from datetime import datetime, timezone, timedelta
from pathlib import Path
from urllib.parse import urlsplit

SYNTHETIC_MARKER = "合成测试夹具；非真实来源；不得用于生产核验"
CST = timezone(timedelta(hours=8))


@dataclass(frozen=True)
class CorpusSource:
    artifact_origin: str
    source_id: str | None
    source_key: str | None
    source_url: str | None
    rights_tag: str | None
    observed_at: datetime | None


def _time(value, *, aware: bool) -> datetime:
    dt = datetime.fromisoformat(str(value))
    if aware and dt.tzinfo is None:
        raise ValueError("合成夹具 observed_at 必须带时区")
    return dt if dt.tzinfo else dt.replace(tzinfo=CST)


def _http(value) -> bool:
    if not isinstance(value, str):
        return False
    parsed = urlsplit(value)
    return parsed.scheme in ("http", "https") and bool(parsed.hostname)


def _unique_pairs(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("夹具 manifest 重复字段")
        value[key] = item
    return value


def fixture_manifest(path: Path) -> dict:
    """共同制品信封闭合；data 由领域包校验，不在 SDK 解释组织数据。"""
    value = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=_unique_pairs)
    if not isinstance(value, dict) or set(value) != {"version", "namespace", "fixture_only", "samples", "data"}:
        raise ValueError("夹具 manifest 字段不闭合")
    if (not isinstance(value["version"], str) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", value["version"])
            or not isinstance(value["namespace"], str) or not re.fullmatch(r"fixture:[a-z0-9:-]+", value["namespace"])
            or value["fixture_only"] is not True):
        raise ValueError("夹具 manifest 范围或版本不支持")
    samples = value["samples"]
    if not isinstance(samples, dict) or not samples or not isinstance(value["data"], dict):
        raise ValueError("夹具 manifest 内容无效")
    source_objects = set()
    for name, item in samples.items():
        if not isinstance(name, str) or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}", name):
            raise ValueError("夹具样本名无效")
        if not isinstance(item, dict) or set(item) != {"source_id", "source_key", "raw_sha256", "rights_tag"}:
            raise ValueError("夹具 manifest 来源字段不闭合")
        if (not isinstance(item["source_id"], str) or not item["source_id"]
                or not isinstance(item["source_key"], str) or not item["source_key"].startswith(value["namespace"] + ":")
                or not isinstance(item["rights_tag"], str) or not item["rights_tag"]):
            raise ValueError("夹具来源身份或权利标签无效")
        identity = (item["source_id"], item["source_key"])
        if identity in source_objects:
            raise ValueError("夹具来源对象键重复")
        source_objects.add(identity)
        if not isinstance(item["raw_sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", item["raw_sha256"]):
            raise ValueError("夹具原件哈希无效")
    return value


def _has_synthetic_marker(raw: bytes) -> bool:
    # 与下游 HTML 相同的可见文本：实体/inline 标签不改变声明的分类。
    # 非 UTF-8 原件保留旧元数据路径；格式选择由共同样本路径边界决定。
    try:
        html = raw.decode("utf-8")
    except UnicodeError:
        return False
    if not html.strip():
        return False
    from lxml.etree import ParserError
    from .testing import html_to_document
    from .document import Document
    try:
        doc = Document(html_to_document(html, "snp_" + "0" * 26))
    except (ValueError, ParserError):
        return False  # 此处不替代下游文档结构/格式校验。
    return any(SYNTHETIC_MARKER in u.text for u in doc.units())


def corpus_source(sample: Path, meta: dict, raw: bytes, *, for_replay: bool = False) -> CorpusSource:
    """校验显式来源种类；旧公开语料的 checker 宽容元数据、replay 完整性要求保留。"""
    if not isinstance(meta, dict):
        raise ValueError("语料来源元数据必须为对象")
    kind = meta.get("artifact_origin", "public_http")
    if kind not in ("public_http", "synthetic_fixture"):
        raise ValueError("不支持的语料 artifact_origin")
    if meta.get("raw_sha256") and hashlib.sha256(raw).hexdigest() != meta["raw_sha256"]:
        raise ValueError(f"{sample.name}: 原件哈希不符")
    if kind == "public_http":
        # 与 checker/replay 选择原件的规则完全相同；HTML 内容前缀不能获得 PDF 豁免。
        pdf = (sample / "raw.pdf").exists()
        if any(k in meta for k in ("fixture_only", "fixture_manifest")) or (not pdf and _has_synthetic_marker(raw)):
            raise ValueError("夹具不能伪装公开来源")
        url = meta.get("source_url")
        if (url is not None and not _http(url)) or (for_replay and (not _http(url) or not meta.get("source_id") or not meta.get("rights_tag"))):
            raise ValueError("样本缺少来源、来源地址或权利标签")
        observed = _time(meta["fetched_at"], aware=False) if "fetched_at" in meta else None
        if for_replay and observed is None:
            raise ValueError("公开语料缺少 fetched_at")
        return CorpusSource(kind, meta.get("source_id"), url, url, meta.get("rights_tag"), observed)
    required = {"artifact_origin", "source_id", "source_key", "fixture_only", "fixture_manifest", "fixture_manifest_sha256", "raw_sha256", "observed_at", "rights_tag", "schema"}
    if not required <= set(meta) or set(meta) - required - {"notes"}:
        raise ValueError("合成夹具元数据字段不闭合或缺失；禁止 URL/fetched_at")
    if meta["fixture_only"] is not True or (not isinstance(meta["fixture_manifest"], str) or not re.fullmatch(r"[a-zA-Z0-9_-]+\.json", meta["fixture_manifest"])):
        raise ValueError("合成夹具必须显式绑定固定 manifest")
    manifest_path = sample.parent / meta["fixture_manifest"]
    if hashlib.sha256(manifest_path.read_bytes()).hexdigest() != meta["fixture_manifest_sha256"]:
        raise ValueError("夹具 manifest 哈希不符")
    manifest = fixture_manifest(manifest_path)
    binding = manifest["samples"].get(sample.name)
    if binding is None or any(meta[k] != binding[k] for k in binding):
        raise ValueError("夹具来源/原件与 manifest 不一致")
    if not (sample / "raw.html").is_file() or (sample / "raw.pdf").exists() or (sample / "normalized.json").exists():
        raise ValueError("合成夹具仅接受原始 HTML")
    if not _has_synthetic_marker(raw):
        raise ValueError("夹具原文缺少可见合成标记")
    return CorpusSource(kind, meta["source_id"], meta["source_key"], None, meta["rights_tag"], _time(meta["observed_at"], aware=True))
