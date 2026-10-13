"""把 contracts/schemas 打包为一份只含 $defs 的规范化 Schema，供三种语言的生成器使用。

- 类型名：顶层 Schema 取 title 的首个词；$defs 取 x-type-name，否则取键名的 PascalCase；重名即报错
- 跨文件 $ref 改写为包内 #/$defs/<类型名>；去掉 $ref 的兄弟关键字（生成器会因此产生重复类型）
- 去掉只用于校验、不影响结构的关键字（if/then/else/not/contains 等）
打包结果只用于生成类型；运行时校验始终使用原始 Schema。
"""
from __future__ import annotations

import copy
from urllib.parse import urldefrag, urljoin

from .repo import DIALECT, Repo

VALIDATION_ONLY = {"if", "then", "else", "not", "contains", "dependentSchemas", "dependentRequired",
                   "unevaluatedProperties", "minContains", "maxContains"}


def pascal(key: str) -> str:
    return "".join(p[:1].upper() + p[1:] for p in key.replace("-", "_").split("_") if p)


class NameClash(Exception):
    pass


def type_names(repo: Repo) -> dict[tuple[str, str], str]:
    """(schema $id, JSON Pointer 片段) → 类型名。"""
    names: dict[tuple[str, str], str] = {}
    owner: dict[str, str] = {}

    def add(sid: str, frag: str, name: str):
        if name in owner and owner[name] != f"{sid}#{frag}":
            raise NameClash(f"类型名 {name} 同时用于 {owner[name]} 与 {sid}#{frag}，请用 x-type-name 区分")
        owner[name] = f"{sid}#{frag}"
        names[(sid, frag)] = name

    for s in repo.schemas.values():
        sid = s["$id"]
        if "properties" in s:
            add(sid, "", s["title"].split()[0])
        for key, d in s.get("$defs", {}).items():
            add(sid, f"/$defs/{key}", d.get("x-type-name") or pascal(key))
    return names


def _clean(node, base: str, names, keep_validation: bool = False):
    if isinstance(node, list):
        return [_clean(x, base, names, keep_validation) for x in node]
    if not isinstance(node, dict):
        return node
    if "$ref" in node:
        url, frag = urldefrag(urljoin(base, node["$ref"]))
        key = (url, frag)
        if key not in names:
            raise KeyError(f"$ref {node['$ref']}（{base}）不指向可命名的类型；请把目标放进 $defs")
        return {"$ref": f"#/$defs/{names[key]}"}
    out = {}
    for k, v in node.items():
        if (k in VALIDATION_ONLY and not keep_validation) or k in ("$id", "$schema", "$defs") or k.startswith("x-"):
            continue
        out[k] = _clean(v, base, names, keep_validation)
    if "allOf" in out:
        out["allOf"] = [x for x in out["allOf"] if x]
        if not out["allOf"]:
            del out["allOf"]
        elif "properties" in out and not keep_validation:
            # 自身属性与 allOf 并列时，部分生成器会丢弃自身属性：把自身属性移入 allOf 的一项
            own = {k: out.pop(k) for k in ("properties", "required", "additionalProperties") if k in out}
            out["allOf"].append({"type": "object", **own})
    return out


def build_bundle(repo: Repo, keep_validation: bool = False) -> dict:
    """keep_validation=True 时保留校验关键字，供兼容性比较使用。"""
    names = type_names(repo)
    defs = {}
    for s in repo.schemas.values():
        sid = s["$id"]
        if "properties" in s:
            root = {k: v for k, v in s.items() if k != "$defs"}
            defs[names[(sid, "")]] = _clean(copy.deepcopy(root), sid, names, keep_validation)
        for key, d in s.get("$defs", {}).items():
            defs[names[(sid, f"/$defs/{key}")]] = _clean(copy.deepcopy(d), sid, names, keep_validation)
    return {"$schema": DIALECT, "title": "MiaoQiaContracts", "description": "生成用打包 Schema", "$defs": dict(sorted(defs.items()))}
