"""事件契约检查：contracts/events/<命名空间>/*.asyncapi.yaml。

  E1  asyncapi 版本为 3.0.0
  E2  消息名为 <命名空间>.<对象>.<动词>.v<主版本>，命名空间与目录一致，全局唯一
  E3  载荷中的 $ref 都能解析（组件内引用或指向 contracts/schemas 的相对引用）
  E4  载荷中的标识前缀已登记
"""
from __future__ import annotations

import json
import copy
import re
from pathlib import Path

from .lint import _prefixes_in, _walk
from .repo import Repo, load_yaml
from .report import Report
from .fixtures import check_cases
from jsonschema import Draft202012Validator

NAME = re.compile(r"^([a-z_]+)\.[a-z_]+\.[a-z_]+\.v[0-9]+$")


def _resolve_pointer(doc, fragment: str):
    node = doc
    for p in fragment.lstrip("/").split("/"):
        if p == "":
            continue
        node = node[p]
    return node


def event_names(repo: Repo) -> set[str]:
    names = set()
    for f in sorted((repo.contracts / "events").glob("*/*.asyncapi.yaml")):
        doc = load_yaml(f)
        for m in (doc.get("components", {}).get("messages") or {}).values():
            names.add(m.get("name"))
    return names


def event_components(repo: Repo, path: Path, doc: dict) -> dict:
    """事件检查和兼容检查共用外部引用归一，保留内部组件引用。"""
    components = copy.deepcopy(doc.get("components", {}))
    for _, node in _walk(components):
        ref = node.get("$ref", "")
        if ref and not ref.startswith("#"):
            target, _, fragment = ref.partition("#")
            try:
                rel = (path.parent / target).resolve().relative_to(repo.schema_dir.resolve())
            except ValueError:
                continue
            if rel in repo.schemas:
                node["$ref"] = repo.schemas[rel]["$id"] + ("#" + fragment if fragment else "")
    return components


def check_events(repo: Repo) -> Report:
    r = Report()
    seen: dict[str, str] = {}
    fixture_count = 0
    files = sorted((repo.contracts / "events").glob("*/*.asyncapi.yaml"))
    for f in files:
        where = str(f.relative_to(repo.root))
        ns = f.parent.name
        doc = load_yaml(f)
        if doc.get("asyncapi") != "3.0.0":
            r.error(where, "E1 asyncapi 必须为 3.0.0")
        msgs = (doc.get("components", {}).get("messages") or {})
        for key, m in msgs.items():
            name = m.get("name", "")
            mm = NAME.match(name)
            if not mm or mm.group(1) != ns:
                r.error(where, f"E2 消息 {key} 名称 {name!r} 不符合 {ns}.<对象>.<动词>.v<n>")
            if name in seen:
                r.error(where, f"E2 消息名 {name} 与 {seen[name]} 重复")
            seen[name] = where
        for path, node in _walk(doc):
            ref = node.get("$ref")
            if isinstance(ref, str):
                target, _, frag = ref.partition("#")
                try:
                    if not target:
                        _resolve_pointer(doc, frag)
                    else:
                        tf = (f.parent / target).resolve()
                        _resolve_pointer(json.loads(tf.read_text(encoding="utf-8")), frag)
                except (KeyError, FileNotFoundError, TypeError) as e:
                    r.error(where, f"E3 {path} 无法解析 $ref {ref}（{e}）")
            pat = node.get("pattern")
            if isinstance(pat, str) and pat.endswith("_[0-9A-HJKMNP-TV-Z]{26}$"):
                for p in _prefixes_in(pat) or []:
                    if p not in repo.prefixes:
                        r.error(where, f"E4 {path} 前缀 {p}_ 未登记")
        components = event_components(repo, f, doc)
        fixture_file = f.parent / "fixtures.yaml"
        covered = set()
        if fixture_file.exists():
            for spec in load_yaml(fixture_file).get("fixtures", []):
                covered.add(spec["event"])
                message = next((m for m in components.get("messages", {}).values() if m.get("name") == spec["event"]), None)
                if message is None:
                    r.error(where, f"E5 样例引用未定义事件 {spec['event']}")
                    continue
                schema = {"$schema": "https://json-schema.org/draft/2020-12/schema", "components": components, **message["payload"]}
                validator = Draft202012Validator(schema, registry=repo.registry, format_checker=Draft202012Validator.FORMAT_CHECKER)
                fixture_count += check_cases(validator, spec, spec["event"], r)
                if not any(not case["valid"] for case in spec.get("cases", [])):
                    r.error(where, f"E5 {spec['event']} 没有载荷反例")
        for message in msgs.values():
            if message.get("x-fixtures-required") and message["name"] not in covered:
                r.error(where, f"E5 {message['name']} 缺少事件载荷正反例")
    r.note(f"事件契约检查：{len(files)} 个文件、{len(seen)} 种事件")
    r.note(f"事件载荷样例：{fixture_count} 个正反例")
    return r
