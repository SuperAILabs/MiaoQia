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
            if kind == "replace" and last not in parent:
                raise ValueError(f"replace目标不存在：{op['path']}")
            parent[last] = op["value"]
        elif kind == "remove":
            del parent[last]
    if kind not in ("add", "remove", "replace"):
        raise ValueError(f"不支持的操作 {kind}")
    return doc


def check_cases(validator, spec: dict, where: str, report: Report) -> int:
    """Schema与事件共用同一补丁/正反例执行器。"""
    cases = [{"name": "基准实例", "valid": True, "patch": []}] + spec.get("cases", [])
    for case in cases:
        try:
            instance = copy.deepcopy(case.get("instance", spec["base"]))
            for op in case.get("patch", []):
                instance = _apply(instance, op)
            errors = list(validator.iter_errors(instance))
        except Exception as exc:  # noqa: BLE001 验证制品错误也应完整报告
            report.error(where, f"「{case['name']}」无法校验：{type(exc).__name__}: {str(exc)[:160]}")
            continue
        if case["valid"] and errors:
            report.error(where, f"「{case['name']}」应通过，实际被拒绝：{errors[0].message[:160]}")
        if not case["valid"] and not errors:
            report.error(where, f"「{case['name']}」应被拒绝，实际通过")
    return len(cases)


def check_fixtures(repo: Repo) -> Report:
    r = Report()
    files = sorted([*(repo.contracts / "tests").glob("*.yaml"), *(repo.contracts / "tests").glob("*.json")])
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
        total += check_cases(v, spec, where, r)
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
    for f in sorted([*(repo.contracts / "tests").glob("*.yaml"), *(repo.contracts / "tests").glob("*.json")]):
        spec = load_yaml(f)
        schema = repo.schemas[Path(spec["schema"])]
        tname = names[(schema["$id"], "")]
        cases = [{"name": "基准实例", "valid": True, "patch": []}] + spec.get("cases", [])
        for i, c in enumerate(x for x in cases if x["valid"]):
            inst = copy.deepcopy(c.get("instance", spec["base"]))
            for op in c.get("patch", []):
                inst = _apply(inst, op)
            (out_dir / f"{tname}__{f.stem}_{i}.json").write_text(json.dumps(inst, ensure_ascii=False), encoding="utf-8")
            n += 1
    return n
