"""信息领域包解析器的注册：按包清单（pack.yaml 的 provides.extractors 与 semantic_validator）加载。

入口必须位于该包的 src/ 下，加载后再核对来源文件，避免清单指向其他包或平台代码；
代码型领域包必须登记领域语义校验（与语料评测路径一致，见 miaoqia.sdk.corpus）。
"""
from __future__ import annotations

import importlib
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

import yaml

from miaoqia.sdk import Extractor
from miaoqia.sdk.entrypoints import ENTRY_PATTERN, resolve_semantic_validator, validate_loaded_semantic_validator


@dataclass(frozen=True)
class PackExtractor:
    pack_id: str
    pack_version: str
    extractor: Extractor
    semantic_validator: Callable

    @property
    def ref(self) -> str:
        return f"{self.extractor.id}@{self.extractor.version}"


def _load_module(pack_dir: Path, module: str):
    root = (pack_dir / "src").resolve()
    expected = (root / Path(*module.split("."))).with_suffix(".py").resolve()
    if not expected.is_relative_to(root) or not expected.is_file():
        raise ValueError(f"{pack_dir.name}: 入口模块不在包内：{module}")
    if str(root) not in sys.path:
        sys.path.append(str(root))
    loaded = importlib.import_module(module)
    if not getattr(loaded, "__file__", None) or Path(loaded.__file__).resolve() != expected:
        raise ValueError(f"{pack_dir.name}: 入口模块来源与登记包不一致：{module}")
    return loaded


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
        validator = getattr(_load_module(manifest.parent, module), name)
        validate_loaded_semantic_validator(validator, source, provides["semantic_validator"])
        for ref in refs:
            if not isinstance(ref, str) or not ENTRY_PATTERN.fullmatch(ref):
                raise ValueError(f"{manifest.parent.name}: 解析器入口格式无效：{ref}")
            module, cls = ref.split(":")
            extractor = getattr(_load_module(manifest.parent, module), cls)()
            for attr in ("id", "version", "fact_types", "applies", "extract"):
                if not hasattr(extractor, attr):
                    raise ValueError(f"{manifest.parent.name}: 解析器 {ref} 缺少 {attr}")
            out.append(PackExtractor(pack["id"], pack["version"], extractor, validator))
    refs = [p.ref for p in out]
    if len(refs) != len(set(refs)):
        raise ValueError("解析器标识与版本重复")
    return out
