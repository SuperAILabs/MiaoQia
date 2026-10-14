"""样例校验：contracts/tests/*.yaml 中的正例必须通过、反例必须被拒绝。

文件格式：
  schema: domains/procurement/notice.schema.json   # 相对 contracts/schemas
  base: {...}                                      # 基准实例（应为正例）
  cases:
    - name: 说明
      valid: false
      patch: [{op: remove, path: /buyer/prov}]      # RFC 6902 的 add / remove / replace
"""
from __future__ import annotations

import copy
from pathlib import Path

from jsonschema import Draft202012Validator

from .repo import Repo, load_yaml
from .report import Report


def _parts(pointer: str) -> list[str]:
    if not pointer.startswith("/"):
        raise ValueError(f"JSON Pointer 必须以 / 开头：{pointer}")
    return [p.replace("~1", "/").replace("~0", "~") for p in pointer[1:].split("/")]


def _apply(doc, op: dict):
    parts = _parts(op["path"])
    parent = doc
    for p in parts[:-1]:
        parent = parent[int(p)] if isinstance(parent, list) else parent[p]
    last = parts[-1]
    kind = op["op"]
    if isinstance(parent, list):
        if kind == "add":
            parent.append(op["value"]) if last == "-" else parent.insert(int(last), op["value"])
        elif kind == "remove":
            del parent[int(last)]
        elif kind == "replace":
            parent[int(last)] = op["value"]
    else:
        if kind in ("add", "replace"):
            parent[last] = op["value"]
        elif kind == "remove":
            del parent[last]
    if kind not in ("add", "remove", "replace"):
        raise ValueError(f"不支持的操作 {kind}")
    return doc


def check_fixtures(repo: Repo) -> Report:
    r = Report()
    files = sorted((repo.contracts / "tests").glob("*.yaml"))
    total = 0
    covered = set()
    for f in files:
        spec = load_yaml(f)
        rel = Path(spec["schema"])
        covered.add(rel)
        schema = repo.schemas.get(rel)
        where = f"tests/{f.name}"
        if schema is None:
            r.error(where, f"Schema 不存在：{rel}")
            continue
        v = Draft202012Validator(schema, registry=repo.registry, format_checker=Draft202012Validator.FORMAT_CHECKER)
        cases = [{"name": "基准实例", "valid": True, "patch": []}] + spec.get("cases", [])
        for c in cases:
            total += 1
            inst = copy.deepcopy(spec["base"])
            try:
                for op in c.get("patch", []):
                    inst = _apply(inst, op)
            except (KeyError, IndexError, ValueError) as e:
                r.error(where, f"「{c['name']}」补丁无法应用：{e}")
                continue
            try:
                errs = list(v.iter_errors(inst))
            except Exception as e:  # noqa: BLE001  Schema 本身有错误（例如 $ref 无法解析）时，报告而不中断
                r.error(where, f"「{c['name']}」无法校验：{type(e).__name__}: {str(e)[:160]}")
                continue
            if c["valid"] and errs:
                r.error(where, f"「{c['name']}」应通过，实际被拒绝：{errs[0].message[:160]}")
            if not c["valid"] and not errs:
                r.error(where, f"「{c['name']}」应被拒绝，实际通过")
    roots = {rel for rel, s in repo.schemas.items() if "properties" in s}
    for rel in sorted(roots - covered):
        r.error(f"schemas/{rel.as_posix()}", "顶层 Schema 没有样例（contracts/tests/）")
    r.note(f"样例校验：{len(files)} 个文件、{total} 个用例")
    return r


def export_valid(repo: Repo, out_dir: Path) -> int:
    """把全部正例导出为 <类型名>__<序号>.json，供生成代码的往返测试使用。"""
    import json

    from .bundle import type_names

    names = type_names(repo)
    out_dir.mkdir(parents=True, exist_ok=True)
    n = 0
    for f in sorted((repo.contracts / "tests").glob("*.yaml")):
        spec = load_yaml(f)
        schema = repo.schemas[Path(spec["schema"])]
        tname = names[(schema["$id"], "")]
        cases = [{"name": "基准实例", "valid": True, "patch": []}] + spec.get("cases", [])
        for i, c in enumerate(x for x in cases if x["valid"]):
            inst = copy.deepcopy(spec["base"])
            for op in c.get("patch", []):
                inst = _apply(inst, op)
            (out_dir / f"{tname}__{f.stem}_{i}.json").write_text(json.dumps(inst, ensure_ascii=False), encoding="utf-8")
            n += 1
    return n
