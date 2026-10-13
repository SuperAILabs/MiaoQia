"""Schema 方言检查：在 JSON Schema 2020-12 之上，约束妙洽契约的写法，保证三语言生成结果稳定。

规则（编号用于报错）：
  S1  $schema 为 2020-12，$id 与文件路径一致
  S2  顶层 Schema 有 title（首个词为 ASCII PascalCase，作为生成类型名）与 description
  S3  不使用 null 类型：字段缺失即未知
  S4  有 properties 的对象必须声明 additionalProperties: false 或 unevaluatedProperties: false
      （以 allOf 混入的公共字段组标 x-mixin: true 豁免）
  S5  标识 pattern 的前缀必须登记在 contracts/identifiers.yaml
  S6  枚举值为 lower_snake_case
  S7  每个 $ref 都能解析
  S8  $ref 不与 allOf 包装组合使用来追加约束（会让生成器产生重复类型）
"""
from __future__ import annotations

import re
from pathlib import Path

from jsonschema import Draft202012Validator
from referencing.exceptions import Unresolvable

from .repo import DIALECT, Repo
from .report import Report

ID_PATTERN = re.compile(r"^\^([a-z]{2,4})_\[0-9A-HJKMNP-TV-Z\]\{26\}\$$")
ID_GROUP_PATTERN = re.compile(r"^\^(?:([a-z]{2,4})|([a-z]+)\[([a-z]+)\])_\[0-9A-HJKMNP-TV-Z\]\{26\}\$$")
SNAKE = re.compile(r"^[a-z][a-z0-9]*(_[a-z0-9]+)*$")
TITLE = re.compile(r"^[A-Z][A-Za-z0-9]*(\s|$)")


def _walk(node, path="#"):
    if isinstance(node, dict):
        yield path, node
        for k, v in node.items():
            yield from _walk(v, f"{path}/{k}")
    elif isinstance(node, list):
        for i, v in enumerate(node):
            yield from _walk(v, f"{path}/{i}")


def _prefixes_in(pattern: str) -> list[str] | None:
    m = ID_GROUP_PATTERN.match(pattern)
    if not m:
        return None
    if m.group(1):
        return [m.group(1)]
    stem, chars = m.group(2), m.group(3)
    return [stem + c for c in chars]


def lint_schemas(repo: Repo) -> Report:
    r = Report()
    for rel, s in repo.schemas.items():
        where = f"schemas/{rel.as_posix()}"
        try:
            Draft202012Validator.check_schema(s)
        except Exception as e:  # noqa: BLE001
            r.error(where, f"不是有效的 2020-12 Schema：{e}")
            continue
        if s.get("$schema") != DIALECT:
            r.error(where, "S1 $schema 必须为 2020-12")
        if s.get("$id") != repo.expected_id(rel):
            r.error(where, f"S1 $id 应为 {repo.expected_id(rel)}")
        is_root = "properties" in s
        if is_root and not TITLE.match(s.get("title", "")):
            r.error(where, "S2 顶层 Schema 的 title 必须以 ASCII PascalCase 类型名开头")
        if not s.get("description"):
            r.error(where, "S2 缺少 description")

        resolver = repo.registry.resolver(base_uri=s["$id"])
        for path, node in _walk(s):
            t = node.get("type")
            if t == "null" or (isinstance(t, list) and "null" in t):
                r.error(where, f"S3 {path} 使用了 null 类型")
            if "properties" in node and node.get("type") == "object" and not node.get("x-mixin"):
                if node.get("additionalProperties") is not False and node.get("unevaluatedProperties") is not False:
                    r.error(where, f"S4 {path} 未禁止额外属性")
            pat = node.get("pattern")
            if isinstance(pat, str) and pat.endswith("_[0-9A-HJKMNP-TV-Z]{26}$") and not path.endswith("/ulid_id"):
                pref = _prefixes_in(pat)
                if pref is None:
                    r.error(where, f"S5 {path} 标识 pattern 写法无法识别：{pat}")
                else:
                    for p in pref:
                        if p not in repo.prefixes:
                            r.error(where, f"S5 {path} 前缀 {p}_ 未登记")
            for v in node.get("enum", []) if isinstance(node.get("enum"), list) else []:
                if isinstance(v, str) and not SNAKE.match(v):
                    r.error(where, f"S6 {path} 枚举值 {v!r} 不是 lower_snake_case")
            ref = node.get("$ref")
            if isinstance(ref, str):
                try:
                    resolver.lookup(ref)
                except Unresolvable as e:
                    r.error(where, f"S7 {path} 无法解析 $ref {ref}：{e}")
            a = node.get("allOf")
            if isinstance(a, list):
                for i, sub in enumerate(a):
                    if isinstance(sub, dict) and set(sub) == {"$ref"} and i + 1 < len(a) and set(a[i + 1]) == {"required"}:
                        r.error(where, f"S8 {path} 以 allOf 包装 $ref 追加约束")
    r.note(f"Schema 方言检查：{len(repo.schemas)} 个文件")
    return r


YAML_BOOL_KEYS = re.compile(r"(^\s*|[{,]\s*)(on|off|yes|no|y|n|true|false)\s*:", re.IGNORECASE)


def lint_yaml_keys(paths: list[Path], root: Path) -> Report:
    """Y1：YAML 1.1 会把 on/off/yes/no 等键解析为布尔值。"""
    r = Report()
    for p in paths:
        for i, line in enumerate(p.read_text(encoding="utf-8").splitlines(), 1):
            code = line.split("#", 1)[0]
            if YAML_BOOL_KEYS.search(code):
                r.error(f"{p.relative_to(root)}:{i}", "Y1 键名会被 YAML 1.1 解析为布尔值，请改名")
    return r
