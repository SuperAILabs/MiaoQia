"""仓库路径与契约文件加载。"""
from __future__ import annotations

import json
from dataclasses import dataclass
from functools import cached_property
from pathlib import Path

import yaml
from referencing import Registry, Resource

SCHEMA_BASE = "https://schemas.miaoqia.com/"
DIALECT = "https://json-schema.org/draft/2020-12/schema"


def find_root(start: Path | None = None) -> Path:
    p = (start or Path.cwd()).resolve()
    for d in [p, *p.parents]:
        if (d / "contracts" / "identifiers.yaml").exists():
            return d
    raise SystemExit("找不到仓库根目录（缺少 contracts/identifiers.yaml）")


@dataclass
class Repo:
    root: Path

    @property
    def contracts(self) -> Path:
        return self.root / "contracts"

    @property
    def schema_dir(self) -> Path:
        return self.contracts / "schemas"

    @cached_property
    def schemas(self) -> dict[Path, dict]:
        """相对 contracts/schemas 的路径 → Schema。"""
        out = {}
        for f in sorted(self.schema_dir.rglob("*.schema.json")):
            out[f.relative_to(self.schema_dir)] = json.loads(f.read_text(encoding="utf-8"))
        return out

    @cached_property
    def registry(self) -> Registry:
        reg = Registry()
        for s in self.schemas.values():
            if "$id" in s:
                reg = reg.with_resource(s["$id"], Resource.from_contents(s))
        return reg

    @cached_property
    def prefixes(self) -> set[str]:
        data = yaml.safe_load((self.contracts / "identifiers.yaml").read_text(encoding="utf-8"))
        return set(data["prefixes"])

    def expected_id(self, rel: Path) -> str:
        return SCHEMA_BASE + rel.as_posix()

    def by_id(self, schema_id: str) -> tuple[Path, dict] | None:
        for rel, s in self.schemas.items():
            if s.get("$id") == schema_id:
                return rel, s
        return None


def load_yaml(path: Path):
    return yaml.safe_load(path.read_text(encoding="utf-8"))
