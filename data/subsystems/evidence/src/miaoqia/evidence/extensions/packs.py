"""信息领域包解析器的注册：按包清单（pack.yaml 的 provides.extractors 与 semantic_validator）加载。

入口必须位于该包的 src/ 下（加载与来源核对见 miaoqia.sdk.entrypoints.load_pack_module）；
代码型领域包必须登记领域语义校验（与语料评测路径一致，见 miaoqia.sdk.corpus）。
"""
from __future__ import annotations

from dataclasses import dataclass
from datetime import date
from pathlib import Path
from typing import Callable, Protocol

import yaml

from miaoqia.acquisition.api.handlers import GroupSelector
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
    """Admit prebuilt browser projections or adapt original responses to HTML."""

    source_id: str
    media_type: str
    to_html: Callable[[bytes], str] | None = None
    live_parse: bool | None = None
    to_selected_html: SelectedHtml | None = None
    browser_projection: bool = False


class SelectedHtml(Protocol):
    def __call__(self, raw: bytes, selector: GroupSelector, *, published_on: date, record_digest: str) -> str: ...


def load_documents(packs_dir: str | Path) -> dict[str, DocumentAdapter]:
    """Sources without an adapter deliver HTML originals. Omitted live_parse preserves pass-through."""
    out: dict[str, DocumentAdapter] = {}
    required = {"source_id", "media_type"}
    entrypoints = {"adapter", "selection_adapter"}
    for manifest in sorted(Path(packs_dir).glob("information_domains/*/pack.yaml")):
        ref = yaml.safe_load(manifest.read_text(encoding="utf-8"))["pack"].get("provides", {}).get("documents")
        if ref is None:
            continue
        for item in yaml.safe_load((manifest.parent / ref).read_text(encoding="utf-8"))["documents"]:
            if not required <= item.keys() <= required | entrypoints | {"live_parse", "browser_projection"}:
                raise ValueError(f"{manifest.parent.name}: invalid document adapter fields")
            if "live_parse" in item and type(item["live_parse"]) is not bool:
                raise ValueError(f"{manifest.parent.name}: document adapter live_parse must be a boolean")
            browser = "browser_projection" in item
            if browser:
                if item["browser_projection"] is not True or entrypoints & item.keys():
                    raise ValueError(f"{manifest.parent.name}: browser_projection must be true and used alone")
                if not isinstance(item["media_type"], str) or not item["media_type"]:
                    raise ValueError(f"{manifest.parent.name}: browser_projection requires an original media_type")
                if "live_parse" not in item:
                    raise ValueError(f"{manifest.parent.name}: browser_projection requires explicit live_parse")
            elif not entrypoints & item.keys():
                raise ValueError(f"{manifest.parent.name}: document adapter requires an adapter entry")
            if "selection_adapter" in item and "live_parse" not in item:
                raise ValueError(f"{manifest.parent.name}: selection_adapter requires explicit live_parse")
            source = item["source_id"]
            if source in out:
                raise ValueError(f"{manifest.parent.name}: duplicate document adapter for {source}")
            functions = {}
            for key in entrypoints & item.keys():
                entry = item[key]
                if not isinstance(entry, str) or not ENTRY_PATTERN.fullmatch(entry):
                    raise ValueError(f"{manifest.parent.name}: invalid document adapter entry {entry!r}")
                module, name = entry.split(":")
                fn = getattr(load_pack_module(manifest.parent, module), name)
                if not callable(fn):
                    raise ValueError(f"{manifest.parent.name}: document adapter entry is not callable")
                functions[key] = fn
            out[source] = DocumentAdapter(source, item["media_type"], functions.get("adapter"),
                                          item.get("live_parse"), functions.get("selection_adapter"), browser)
    return out
