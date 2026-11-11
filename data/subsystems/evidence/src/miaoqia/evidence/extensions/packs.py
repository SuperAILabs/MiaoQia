"""信息领域包解析器的注册：按包清单（pack.yaml 的 provides.extractors 与 semantic_validator）加载。

入口必须位于该包的 src/ 下（加载与来源核对见 miaoqia.sdk.entrypoints.load_pack_module）；
代码型领域包必须登记领域语义校验（与语料评测路径一致，见 miaoqia.sdk.corpus）。
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Callable

import yaml

from miaoqia.sdk import Extractor
from miaoqia.sdk.entrypoints import (ENTRY_PATTERN, load_pack_module, resolve_semantic_validator,
                                     validate_loaded_semantic_validator)


@dataclass(frozen=True)
class PackExtractor:
    pack_id: str
    pack_version: str
    extractor: Extractor
    semantic_validator: Callable

    @property
    def ref(self) -> str:
        return f"{self.extractor.id}@{self.extractor.version}"


def load(packs_dir: str | Path) -> list[PackExtractor]:
    out: list[PackExtractor] = []
    for manifest in sorted(Path(packs_dir).glob("information_domains/*/pack.yaml")):
        pack = yaml.safe_load(manifest.read_text(encoding="utf-8"))["pack"]
        provides = pack.get("provides", {})
        refs = provides.get("extractors", [])
        if not refs:
            continue
        if "semantic_validator" not in provides:
            raise ValueError(f"{manifest.parent.name}: 代码型领域包未登记 semantic_validator")
        module, name, source = resolve_semantic_validator(manifest.parent, provides["semantic_validator"])
        validator = getattr(load_pack_module(manifest.parent, module), name)
        validate_loaded_semantic_validator(validator, source, provides["semantic_validator"])
        for ref in refs:
            if not isinstance(ref, str) or not ENTRY_PATTERN.fullmatch(ref):
                raise ValueError(f"{manifest.parent.name}: 解析器入口格式无效：{ref}")
            module, cls = ref.split(":")
            extractor = getattr(load_pack_module(manifest.parent, module), cls)()
            for attr in ("id", "version", "fact_types", "applies", "extract"):
                if not hasattr(extractor, attr):
                    raise ValueError(f"{manifest.parent.name}: 解析器 {ref} 缺少 {attr}")
            out.append(PackExtractor(pack["id"], pack["version"], extractor, validator))
    refs = [p.ref for p in out]
    if len(refs) != len(set(refs)):
        raise ValueError("解析器标识与版本重复")
    return out


@dataclass(frozen=True)
class DocumentAdapter:
    """How to get the announcement HTML out of a live source's original response (pack.yaml provides.documents)."""

    source_id: str
    media_type: str
    to_html: Callable[[bytes], str]


def load_documents(packs_dir: str | Path) -> dict[str, DocumentAdapter]:
    """Sources without an adapter deliver HTML originals."""
    out: dict[str, DocumentAdapter] = {}
    for manifest in sorted(Path(packs_dir).glob("information_domains/*/pack.yaml")):
        ref = yaml.safe_load(manifest.read_text(encoding="utf-8"))["pack"].get("provides", {}).get("documents")
        if ref is None:
            continue
        for item in yaml.safe_load((manifest.parent / ref).read_text(encoding="utf-8"))["documents"]:
            if set(item) != {"source_id", "media_type", "adapter"}:
                raise ValueError(f"{manifest.parent.name}: document adapter fields must be source_id, media_type, adapter")
            entry = item["adapter"]
            if not isinstance(entry, str) or not ENTRY_PATTERN.fullmatch(entry):
                raise ValueError(f"{manifest.parent.name}: invalid document adapter entry {entry!r}")
            module, name = entry.split(":")
            fn = getattr(load_pack_module(manifest.parent, module), name)
            if not callable(fn) or item["source_id"] in out:
                raise ValueError(f"{manifest.parent.name}: invalid or duplicate document adapter for {item['source_id']}")
            out[item["source_id"]] = DocumentAdapter(item["source_id"], item["media_type"], fn)
    return out
