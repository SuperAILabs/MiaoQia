"""冻结记录（contracts/reviews/*.yaml）的哈希核对。

  F1 记录缺少 frozen_at / accepted_at / amended_at，无法排序
  F2 受约束文件缺失
  F3 受约束文件与最晚记录的哈希不同，且版本号没有高于冻结版本
  F4 受约束文件读不出版本号

同一路径以时间最晚的记录为准。files、version_owners、reused_frozen_dependencies 是冻结的契约本身，
内容变化须升版本（升到什么程度由 mqc compat 把关）；其余段落（佐证、样例、生成物）是冻结时的证据快照，
允许随后演进，偏离只报告不阻断。
"""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path

from .compat import _semver
from .repo import Repo, load_yaml
from .report import Report

STRICT = {"files", "version_owners", "reused_frozen_dependencies"}
TIME_KEYS = ("frozen_at", "accepted_at", "amended_at")


@dataclass
class Entry:
    record: str
    at: str
    section: str
    path: str
    sha256: str
    version: str | None
    version_owner: str | None

    @property
    def strict(self) -> bool:
        return self.section in STRICT


def _walk(node, section: str, out: list[tuple[str, dict]]) -> None:
    if isinstance(node, dict):
        if isinstance(node.get("path"), str) and any(k.endswith("sha256") for k in node):
            out.append((section, node))
        for v in node.values():
            _walk(v, section, out)
    elif isinstance(node, list):
        for v in node:
            _walk(v, section, out)


def load_entries(repo: Repo, r: Report) -> list[Entry]:
    entries = []
    for f in sorted((repo.contracts / "reviews").glob("*.yaml")):
        rel = f.relative_to(repo.root).as_posix()
        doc = load_yaml(f)
        at = next((str(doc[k]) for k in TIME_KEYS if k in doc), None)
        if at is None:
            r.error(rel, "F1 缺少 frozen_at / accepted_at / amended_at")
            continue
        found: list[tuple[str, dict]] = []
        for section, node in doc.items():
            _walk(node, section, found)
        for section, n in found:
            key = "sha256" if "sha256" in n else next(k for k in n if k.endswith("sha256"))
            ver = n.get("version", n.get("current_version"))
            entries.append(Entry(rel, at, section, n["path"], n[key], str(ver) if ver else None, n.get("version_owner")))
    return entries


def file_version(root: Path, path: str) -> str | None:
    p = root / path
    if not p.is_file():
        return None
    if p.suffix == ".json":
        return json.loads(p.read_text(encoding="utf-8")).get("x-version")
    if p.suffix in (".yaml", ".yml"):
        d = load_yaml(p)
        if isinstance(d, dict):
            for v in (d.get("pack", {}).get("version"), d.get("info", {}).get("version"), d.get("version")):
                if isinstance(v, str):
                    return v
    return None


def check_freeze(repo: Repo) -> Report:
    r = Report()
    by_path: dict[str, list[Entry]] = {}
    for e in load_entries(repo, r):
        by_path.setdefault(e.path, []).append(e)
    drift = 0
    for path, es in sorted(by_path.items()):
        es.sort(key=lambda e: e.at)
        latest = es[-1]
        strict = [e for e in es if e.strict]
        p = repo.root / path
        if not p.is_file():
            if strict:
                r.error(path, f"F2 受约束文件缺失（{strict[-1].record}）")
            else:
                drift += 1
                r.note(f"仅报告：{path} 不存在（{latest.record}）")
            continue
        if hashlib.sha256(p.read_bytes()).hexdigest() == latest.sha256:
            continue
        if not strict:
            drift += 1
            r.note(f"仅报告：{path} 与 {latest.record}（{latest.section}）不同")
            continue
        base = strict[-1]
        owner = base.version_owner or path
        cur = file_version(repo.root, owner)
        if cur is None or base.version is None:
            r.error(path, f"F4 无法读出版本号（{owner}），不能判断是否已升版本")
        elif _semver(cur) <= _semver(base.version):
            r.error(path, f"F3 内容与 {latest.record} 不同，但版本仍为 {cur}（冻结 {base.version}）；"
                          "升版本或补修订记录")
    r.note(f"冻结记录：{len(by_path)} 个路径，{drift} 个仅报告的偏离")
    return r
