"""契约兼容性检查：比较基线（通常是目标分支）与当前工作区。

兼容的含义：新 Schema 必须接受所有按旧 Schema 合法的数据（领域事实长期存储并可重放）。
变更分三级：
  breaking  新 Schema 会拒绝旧数据：删除字段、新增必填、删除枚举值、收紧约束、改变类型或引用、
            修改条件约束（if/then/else 等，保守判定）、删除类型
  additive  新增可选字段、新增枚举值、放宽约束、新增类型（消费者须容忍未知字段与枚举值）
  doc       只改说明、标题、默认值或注解
版本要求：
  Schema 文件（x-version）  breaking → 1.0 起升主版本，0.x 升次版本；additive → 升次版本；doc 不要求。
                            被改动类型的所在文件，以及引用到它的全部顶层 Schema 都须满足。
  事件（AsyncAPI）           已发布事件类型不允许 breaking 变更，也不允许删除；须发布新主版本 .v<n+1>。
  标识前缀                  不允许删除或改变含义（分配后永不复用）。
  扩展包                    包内文件有变化须升 pack.version；策略模板内容变化须升模板 version。
"""
from __future__ import annotations

import io
import subprocess
import tarfile
import tempfile
from dataclasses import dataclass
from pathlib import Path

from .bundle import build_bundle, type_names
from .repo import Repo, load_yaml
from .report import Report

LEVELS = {"doc": 0, "additive": 1, "breaking": 2}
DOC_KEYS = {"description", "title", "default", "examples", "$comment"}
CONDITIONAL = {"if", "then", "else", "not", "contains", "dependentRequired", "dependentSchemas",
               "unevaluatedProperties", "anyOf", "oneOf", "minContains", "maxContains"}
LOWER = {"minimum", "exclusiveMinimum", "minLength", "minItems", "minProperties"}   # 增大即收紧
UPPER = {"maximum", "exclusiveMaximum", "maxLength", "maxItems", "maxProperties"}   # 减小即收紧


@dataclass
class Change:
    level: str
    where: str
    what: str

    def __str__(self) -> str:
        return f"[{self.level}] {self.where}：{self.what}"


def _types(node) -> set | None:
    t = node.get("type")
    if t is None:
        return None
    return set(t) if isinstance(t, list) else {t}


def diff(old, new, where: str) -> list[Change]:
    """比较两个 Schema 节点（已打包、引用为 #/$defs/<类型名>）。"""
    out: list[Change] = []
    if not isinstance(old, dict) or not isinstance(new, dict):
        if old != new:
            out.append(Change("breaking", where, "结构改变"))
        return out
    keys = set(old) | set(new)
    for k in sorted(keys):
        a, b = old.get(k), new.get(k)
        if a == b:
            continue
        w = f"{where}/{k}"
        if k in DOC_KEYS or k.startswith("x-"):
            out.append(Change("doc", w, "说明变化"))
        elif k == "$ref":
            out.append(Change("breaking", w, f"引用由 {a} 改为 {b}"))
        elif k == "type":
            ta, tb = _types(old) or set(), _types(new) or set()
            if a is None:
                out.append(Change("breaking", w, f"新增类型限制 {sorted(tb)}"))
            elif b is None or ta < tb:
                out.append(Change("additive", w, f"类型放宽为 {sorted(tb)}"))
            else:
                out.append(Change("breaking", w, f"类型由 {sorted(ta)} 改为 {sorted(tb)}"))
        elif k == "enum":
            ea, eb = set(a or []), set(b or [])
            if a is None:
                out.append(Change("breaking", w, "新增枚举限制"))
            elif b is None:
                out.append(Change("additive", w, "取消枚举限制"))
            else:
                if ea - eb:
                    out.append(Change("breaking", w, f"删除枚举值 {sorted(ea - eb)}"))
                if eb - ea:
                    out.append(Change("additive", w, f"新增枚举值 {sorted(eb - ea)}（消费者须容忍未知值）"))
        elif k == "required":
            ra, rb = set(a or []), set(b or [])
            if rb - ra:
                out.append(Change("breaking", w, f"新增必填 {sorted(rb - ra)}"))
            if ra - rb:
                out.append(Change("additive", w, f"取消必填 {sorted(ra - rb)}"))
        elif k == "properties":
            pa, pb = a or {}, b or {}
            for p in sorted(set(pa) - set(pb)):
                out.append(Change("breaking", f"{w}/{p}", "删除字段"))
            for p in sorted(set(pb) - set(pa)):
                out.append(Change("additive", f"{w}/{p}", "新增字段"))
            for p in sorted(set(pa) & set(pb)):
                out += diff(pa[p], pb[p], f"{w}/{p}")
        elif k in ("items", "additionalItems", "prefixItems"):
            if a is None or b is None:
                out.append(Change("breaking" if a is None else "additive", w, "数组项约束" + ("新增" if a is None else "取消")))
            else:
                out += diff(a, b, w) if isinstance(a, dict) else [Change("breaking", w, "数组项约束改变")]
        elif k == "allOf":
            if isinstance(a, list) and isinstance(b, list) and len(a) == len(b):
                for i, (x, y) in enumerate(zip(a, b)):
                    out += diff(x, y, f"{w}/{i}")
            else:
                out.append(Change("breaking", w, "allOf 组成改变（需人工确认）"))
        elif k == "additionalProperties":
            if a is False and b is not False:
                out.append(Change("additive", w, "允许额外字段"))
            else:
                out.append(Change("breaking", w, "额外字段约束收紧"))
        elif k in LOWER or k in UPPER:
            if a is None:
                out.append(Change("breaking", w, f"新增约束 {k}={b}"))
            elif b is None:
                out.append(Change("additive", w, f"取消约束 {k}"))
            elif (k in LOWER and b > a) or (k in UPPER and b < a):
                out.append(Change("breaking", w, f"{k} 由 {a} 收紧为 {b}"))
            else:
                out.append(Change("additive", w, f"{k} 由 {a} 放宽为 {b}"))
        elif k in ("pattern", "format", "const", "multipleOf"):
            out.append(Change("additive" if b is None else "breaking", w, f"{k} 由 {a!r} 改为 {b!r}"))
        elif k in CONDITIONAL:
            out.append(Change("breaking", w, "条件约束改变（保守判定，需人工确认）"))
        else:
            out.append(Change("breaking", w, f"{k} 改变（未识别的关键字，保守判定）"))
    return out


def _semver(v: str) -> tuple[int, int, int]:
    a, b, c = (int(x) for x in v.split("."))
    return a, b, c


def bump_ok(old: str, new: str, level: str) -> bool:
    o, n = _semver(old), _semver(new)
    if level == "doc":
        return n >= o
    if level == "breaking" and o[0] >= 1:
        return n[0] > o[0]
    return n[:2] > o[:2]   # additive，或 0.x 阶段的 breaking：至少升次版本


def _refs(node) -> set[str]:
    out = set()
    if isinstance(node, dict):
        if isinstance(node.get("$ref"), str) and node["$ref"].startswith("#/$defs/"):
            out.add(node["$ref"].split("/")[-1])
        for v in node.values():
            out |= _refs(v)
    elif isinstance(node, list):
        for v in node:
            out |= _refs(v)
    return out


def _file_of(repo: Repo) -> dict[str, Path]:
    by_id = {s["$id"]: rel for rel, s in repo.schemas.items()}
    return {name: by_id[sid] for (sid, _frag), name in type_names(repo).items()}


def compare_schemas(base: Repo, head: Repo) -> tuple[Report, list[Change]]:
    r = Report()
    ob = build_bundle(base, keep_validation=True)["$defs"]
    nb = build_bundle(head, keep_validation=True)["$defs"]
    files_o, files_n = _file_of(base), _file_of(head)
    changes: list[Change] = []
    level_by_type: dict[str, str] = {}
    for name in sorted(set(ob) | set(nb)):
        if name not in nb:
            c = [Change("breaking", name, "删除类型")]
        elif name not in ob:
            c = [Change("additive", name, "新增类型")]
        else:
            c = diff(ob[name], nb[name], name)
        if c:
            changes += c
            level_by_type[name] = max((x.level for x in c), key=LEVELS.get)
    # 需要升版本的文件：类型所在文件 + 传递引用到它的顶层 Schema
    graph = {n: _refs(d) for n, d in nb.items()}
    roots = {n for n, f in files_n.items() if "properties" in head.schemas[f] and head.schemas[f]["title"].split()[0] == n}

    def reaches(root: str, target: str, seen=None) -> bool:
        seen = seen or set()
        if root == target:
            return True
        seen.add(root)
        return any(reaches(x, target, seen) for x in graph.get(root, ()) if x not in seen)

    need: dict[Path, str] = {}
    for t, lvl in level_by_type.items():
        f = files_n.get(t) or files_o.get(t)
        targets = {f} | {files_n[rt] for rt in roots if t in nb and reaches(rt, t)}
        for tf in targets:
            need[tf] = max(lvl, need.get(tf, "doc"), key=LEVELS.get)
    for f, lvl in sorted(need.items()):
        if f not in base.schemas or f not in head.schemas:
            continue
        ov, nv = base.schemas[f].get("x-version", "0.0.0"), head.schemas[f].get("x-version", "0.0.0")
        if not bump_ok(ov, nv, lvl):
            want = "主版本" if lvl == "breaking" and _semver(ov)[0] >= 1 else ("次版本" if lvl != "doc" else "不降低版本")
            r.error(f"schemas/{f.as_posix()}", f"存在 {lvl} 变更，x-version 须升{want}（基线 {ov}，当前 {nv}）")
    for f in sorted(set(base.schemas) - set(head.schemas)):
        r.note(f"删除的 Schema 文件：{f}（其类型的删除已按 breaking 记录）")
    return r, changes


def _messages(repo: Repo) -> dict[str, dict]:
    out = {}
    for f in sorted((repo.contracts / "events").glob("*/*.asyncapi.yaml")):
        doc = load_yaml(f)
        comps = doc.get("components", {})
        for m in (comps.get("messages") or {}).values():
            out[m["name"]] = _inline(m.get("payload", {}), comps)
    return out


def _inline(node, comps, depth=0):
    if depth > 20:
        return node
    if isinstance(node, list):
        return [_inline(x, comps, depth + 1) for x in node]
    if not isinstance(node, dict):
        return node
    ref = node.get("$ref")
    if isinstance(ref, str) and ref.startswith("#/components/schemas/"):
        return _inline(comps["schemas"][ref.split("/")[-1]], comps, depth + 1)
    if isinstance(ref, str) and "#/$defs/" in ref:
        return {"$ref": "#/$defs/" + ref.split("#/$defs/")[-1]}
    return {k: _inline(v, comps, depth + 1) for k, v in node.items()}


def compare_events(base: Repo, head: Repo) -> tuple[Report, list[Change]]:
    r = Report()
    ob, nb = _messages(base), _messages(head)
    changes: list[Change] = []
    for name in sorted(set(ob) - set(nb)):
        r.error("events", f"删除了已发布的事件类型 {name}：须先并行发布新版本并经过过渡期")
    for name in sorted(set(nb) - set(ob)):
        changes.append(Change("additive", name, "新增事件类型"))
    for name in sorted(set(ob) & set(nb)):
        c = diff(ob[name], nb[name], name)
        changes += c
        for x in c:
            if x.level == "breaking":
                r.error("events", f"{x}；已发布事件不允许不兼容变更，请发布 .v<n+1>")
    return r, changes


def compare_identifiers(base: Repo, head: Repo) -> Report:
    r = Report()
    ob = load_yaml(base.contracts / "identifiers.yaml")["prefixes"]
    nb = load_yaml(head.contracts / "identifiers.yaml")["prefixes"]
    for p in sorted(set(ob) - set(nb)):
        r.error("identifiers.yaml", f"删除了前缀 {p}_：前缀分配后永不复用，不得删除")
    for p in sorted(set(ob) & set(nb)):
        if ob[p].get("object") != nb[p].get("object"):
            r.error("identifiers.yaml", f"前缀 {p}_ 的对象由 {ob[p].get('object')} 改为 {nb[p].get('object')}")
    return r


def _tracked(root: Path) -> set[Path] | None:
    """工作区中未被 .gitignore 排除的文件（含未提交的新文件）；root 不是 git 工作区时返回 None。"""
    res = subprocess.run(["git", "ls-files", "-co", "--exclude-standard", "-z", "packs"], cwd=root, capture_output=True)
    if res.returncode != 0:
        return None
    return {root / f for f in res.stdout.decode().split("\0") if f}


def _pack_files(repo: Repo) -> dict[Path, dict[str, bytes]]:
    keep = _tracked(repo.root)  # 基线由 git archive 解出，不含被忽略的文件；工作区需排除 __pycache__ 等
    out = {}
    for pf in sorted((repo.root / "packs").glob("*/*/pack.yaml")):
        d = pf.parent
        out[d.relative_to(repo.root)] = {str(f.relative_to(d)): f.read_bytes() for f in sorted(d.rglob("*"))
                                         if f.is_file() and (keep is None or f in keep)}
    return out


def compare_packs(base: Repo, head: Repo) -> Report:
    r = Report()
    ob, nb = _pack_files(base), _pack_files(head)
    for d in sorted(set(ob) & set(nb)):
        if ob[d] == nb[d]:
            continue
        ov = load_yaml(base.root / d / "pack.yaml")["pack"]["version"]
        nv = load_yaml(head.root / d / "pack.yaml")["pack"]["version"]
        if _semver(nv) <= _semver(ov):
            r.error(str(d), f"包内容有变化，pack.version 须提升（基线 {ov}，当前 {nv}）")
        for f in sorted(set(ob[d]) & set(nb[d])):
            if f.startswith("policy/templates/") and ob[d][f] != nb[d][f]:
                tv_o = load_yaml(base.root / d / f)["template"]["version"]
                tv_n = load_yaml(head.root / d / f)["template"]["version"]
                if _semver(tv_n) <= _semver(tv_o):
                    r.error(f"{d}/{f}", f"模板内容有变化，template.version 须提升（基线 {tv_o}，当前 {tv_n}）；已激活策略固定模板版本")
    return r


def checkout(root: Path, ref: str, dest: Path) -> bool:
    """把基线 ref 中的 contracts/ 与 packs/ 解出到 dest；基线中没有契约目录时返回 False。"""
    res = subprocess.run(["git", "archive", "--format=tar", ref, "contracts", "packs"], cwd=root, capture_output=True)
    if res.returncode != 0:
        if b"did not match any files" in res.stderr or b"pathspec" in res.stderr:
            return False
        raise RuntimeError(f"git archive {ref} 失败：{res.stderr.decode(errors='replace')}")
    with tarfile.open(fileobj=io.BytesIO(res.stdout)) as tf:
        tf.extractall(dest, filter="data")
    return (dest / "contracts" / "identifiers.yaml").exists()


def compat(head: Repo, base_ref: str, verbose: bool) -> Report:
    r = Report()
    with tempfile.TemporaryDirectory() as tmp:
        if not checkout(head.root, base_ref, Path(tmp)):
            r.note(f"基线 {base_ref} 中没有契约，跳过兼容性检查")
            return r
        base = Repo(Path(tmp))
        rs, cs = compare_schemas(base, head)
        re_, ce = compare_events(base, head)
        r.extend(rs); r.extend(re_)
        r.extend(compare_identifiers(base, head))
        r.extend(compare_packs(base, head))
        allc = cs + ce
        counts = {lvl: sum(1 for c in allc if c.level == lvl) for lvl in LEVELS}
        r.note(f"兼容性检查（基线 {base_ref}）：breaking {counts['breaking']}、additive {counts['additive']}、doc {counts['doc']}")
        for c in allc if verbose else [c for c in allc if c.level == "breaking"]:
            r.note(f"  {c}")
    return r
