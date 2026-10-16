"""扩展包一致性检查：packs/ 中的声明与 contracts/ 对齐。

信息领域包：
  P1  pack.yaml 声明的 Schema、事件文件与领域校验入口存在且合规；含代码的包必须登记领域校验入口
  P2  生命周期状态机的状态与触发值都取自所引用 Schema 的枚举，所有状态可到达
  P8  关系类型定义（provides.relationships）：类型唯一，端点与约束取自登记的种类，
      抽取来源的 Schema 存在、端点与约束路径在 Schema 中存在
应用领域包（策略，语义见 docs/03-architecture/11-policy-dsl.md）：
  P0  模板文件符合 contracts/policy/policy.schema.json
  P3  视图投影的来源 Schema 存在
  P4  模板引用的事实类型、视图项、特征、参数、档案字段、原因代码都已声明
  P5  规则编号前缀与子句一致且不重复；排除规则必须有原因代码
  P6  表达式与视图 triggers 中出现的事件类型已在 contracts/events 中定义
  P7  样例的期望处置取值合法
"""
from __future__ import annotations

import importlib.util
import json
import re
from pathlib import Path

from jsonschema import Draft202012Validator

from .events import event_names
from .repo import Repo, find_root, load_yaml
from .report import Report

W = r"([A-Za-z0-9_]+)"
CLAUSES = {"require": "R-", "exclude": "X-", "watch": "W-", "boost": "B-", "penalize": "P-"}
DISPOSITIONS = {"pursue_candidate", "review_candidate", "watch_signal", "excluded", "no_signal"}
EVENT_LITERAL = re.compile(r"([a-z_]+\.[a-z_]+\.[a-z_]+\.v[0-9]+)")


ENDPOINT_KINDS = {"organization", "project", "location", "category", "brand"}   # entity-graph 实体
QUALIFIER_KINDS = ENDPOINT_KINDS | {"domain_fact", "lot", "region", "period"}
REL_TYPE = re.compile(r"^[a-z][a-z0-9_]*$")


def _schema_node(repo: Repo, schema_rel: str, path: str):
    """按 a.b[*].c 路径在 Schema 中逐级查找属性（解析 $ref 与 allOf），找不到返回 None。"""
    s = repo.schemas.get(Path(schema_rel.removeprefix("contracts/schemas/")))
    if s is None:
        return None

    def deref(n, res):
        while isinstance(n, dict) and "$ref" in n:
            hit = res.lookup(n["$ref"])
            n, res = hit.contents, hit.resolver
        return n, res

    node, res = s, repo.registry.resolver(base_uri=s["$id"])
    for part in path.split("."):
        name, many = (part[:-3], True) if part.endswith("[*]") else (part, False)
        node, res = deref(node, res)
        props = {k: (v, res) for k, v in node.get("properties", {}).items()}
        for sub in node.get("allOf", []):
            sub, sres = deref(sub, res)
            props.update({k: (v, sres) for k, v in sub.get("properties", {}).items()})
        if name not in props:
            return None
        node, res = deref(*props[name])
        if many:
            if node.get("type") != "array":
                return None
            node, res = deref(node.get("items", {}), res)
    return node


def check_relationships(repo: Repo, pack_dir: Path, rel_file: str) -> Report:
    r = Report()
    where = str((pack_dir / rel_file).relative_to(repo.root))
    path = pack_dir / rel_file
    if not path.exists():
        r.error(where, "P8 关系定义文件不存在")
        return r
    seen = set()
    for d in load_yaml(path).get("relationships", []):
        t = d.get("type", "")
        if not REL_TYPE.match(t) or t in seen:
            r.error(where, f"P8 关系类型 {t!r} 不合法或重复")
        seen.add(t)
        for end in ("from", "to"):
            if d.get(end) not in ENDPOINT_KINDS:
                r.error(where, f"P8 {t} 的端点 {end}={d.get(end)!r} 不是实体图谱实体种类")
        for q in d.get("qualifiers", []):
            if q not in QUALIFIER_KINDS:
                r.error(where, f"P8 {t} 的约束 {q!r} 不是登记的约束种类")
        if not d.get("sources"):
            r.error(where, f"P8 {t} 缺少抽取来源")
        for src in d.get("sources", []):
            schema = src.get("schema", "")
            if schema and Path(schema.removeprefix("contracts/schemas/")) not in repo.schemas:
                r.error(where, f"P8 {t} 的来源 Schema 不存在：{schema}")
                continue
            for key in ("from_path", "to_path", *(f"qualifier_paths.{q}" for q in src.get("qualifier_paths", {}))):
                val = src.get(key) if "." not in key else src["qualifier_paths"][key.split(".", 1)[1]]
                if val is None:
                    if key in ("from_path", "to_path") and schema:
                        r.error(where, f"P8 {t} 的来源缺少 {key}")
                    continue
                if schema and _schema_node(repo, schema, val) is None:
                    r.error(where, f"P8 {t} 的路径 {val} 不在 {schema} 中")
            for q in src.get("qualifier_paths", {}):
                if q not in d.get("qualifiers", []):
                    r.error(where, f"P8 {t} 的来源给出了未声明的约束 {q}")
    return r


def _enum_at(repo: Repo, schema_rel: str, field: str) -> set[str] | None:
    s = repo.schemas.get(Path(schema_rel))
    if s is None:
        return None
    node = s["properties"].get(field)
    if node is None:
        return None
    if "$ref" in node:
        res = repo.registry.resolver(base_uri=s["$id"]).lookup(node["$ref"]).contents
        node = res
    return set(node.get("enum", []))


_ENTRYPOINTS = None


def _entrypoints():
    """入口登记规则唯一定义在 SDK；从当前检出按文件路径加载（与 tools/dev/gen_local.py 加载 kernel 同法），契约工具不依赖 SDK 包。"""
    global _ENTRYPOINTS
    if _ENTRYPOINTS is None:
        source = find_root() / "data/sdk/src/miaoqia/sdk/entrypoints.py"
        spec = importlib.util.spec_from_file_location("mq_sdk_entrypoints", source)
        _ENTRYPOINTS = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(_ENTRYPOINTS)
    return _ENTRYPOINTS


def check_information_pack(repo: Repo, pack_dir: Path) -> Report:
    r = Report()
    where = str(pack_dir.relative_to(repo.root))
    pack = load_yaml(pack_dir / "pack.yaml")["pack"]
    prov = pack.get("provides", {})
    for sp in prov.get("schemas", []):
        if not (repo.root / sp).exists():
            r.error(where, f"P1 Schema 不存在：{sp}")
    if "events" in prov and not (repo.root / prov["events"]).exists():
        r.error(where, f"P1 事件文件不存在：{prov['events']}")
    if "semantic_validator" in prov:
        try:
            _entrypoints().resolve_semantic_validator(pack_dir, prov["semantic_validator"])
        except ValueError as exc:
            r.error(where, f"P1 {exc}")
    elif (pack_dir / "src").exists():
        r.error(where, "P1 代码型领域包必须登记semantic_validator")
    if "relationships" in prov:
        r.extend(check_relationships(repo, pack_dir, prov["relationships"]))
    if "lifecycle" in prov:
        lc = load_yaml(pack_dir / prov["lifecycle"])["lifecycle"]
        states = _enum_at(repo, lc["schema"], lc["field"])
        triggers = _enum_at(repo, lc["trigger_source"]["schema"], lc["trigger_source"]["field"])
        if states is None or triggers is None:
            r.error(where, "P2 生命周期引用的 Schema 或字段不存在")
            return r
        reached, used = set(), set()
        for t in lc["initial"]:
            if t["to"] not in states:
                r.error(where, f"P2 初始状态 {t['to']} 不在 {lc['field']} 枚举中")
            reached.add(t["to"]); used |= set(t["trigger"])
        conditions = [t.get("when", {}) for t in lc["initial"] + lc["transitions"]]
        conditions.append(lc.get("current_projection_filter", {}))
        for condition in conditions:
            try:
                Draft202012Validator.check_schema(condition)
            except Exception as exc:  # noqa: BLE001 契约条件错误应报告
                r.error(where, f"P2 生命周期when不是有效JSON Schema：{exc}")
        for t in lc["transitions"]:
            fr = t["from"] if isinstance(t["from"], list) else [t["from"]]
            for s in fr + [t["to"]]:
                if s not in states:
                    r.error(where, f"P2 状态 {s} 不在 {lc['field']} 枚举中")
            reached.add(t["to"]); used |= set(t.get("trigger", []))
        for s in sorted(states - reached):
            r.error(where, f"P2 状态 {s} 不可到达")
        for t in sorted(used - triggers):
            r.error(where, f"P2 触发值 {t} 不在 {lc['trigger_source']['field']} 枚举中")
    return r


def _template_validator(repo: Repo) -> Draft202012Validator:
    schema = json.loads((repo.contracts / "policy" / "policy.schema.json").read_text(encoding="utf-8"))
    return Draft202012Validator(schema, format_checker=Draft202012Validator.FORMAT_CHECKER)


def check_application_pack(repo: Repo, pack_dir: Path) -> Report:
    r = Report()
    pack = load_yaml(pack_dir / "pack.yaml")["pack"]
    pol = pack.get("provides", {}).get("policy")
    if not pol:
        return r
    views = load_yaml(pack_dir / pol["views"])
    feats = load_yaml(pack_dir / pol["features"])["features"]
    reasons = load_yaml(pack_dir / pol["reason_codes"])["reason_codes"]
    items = set(views.get("view_items", {}))
    item_roots = {i.split(".")[0] for i in items}
    pf = set(views.get("profile_fields", {}))
    fact_types = views["fact_types"]
    events = event_names(repo)
    where_v = str((pack_dir / pol["views"]).relative_to(repo.root))
    for ft, d in fact_types.items():
        src = d.get("source", "")
        if src.startswith("contracts/") and not (repo.root / src).exists():
            r.error(where_v, f"P3 {ft} 的来源 Schema 不存在：{src}")
        elif not src.startswith("contracts/"):
            r.error(where_v, f"P3 {ft} 没有正式来源 Schema：{src}")
        for ev in d.get("triggers", []):
            if ev not in events:
                r.error(where_v, f"P6 {ft} 的触发事件 {ev} 未在 contracts/events 中定义")

    def fact_field_ok(fld: str, fts) -> bool:
        return fld in item_roots or any(fld in fact_types[t]["fields"] for t in fts if t in fact_types)

    for name, f in feats.items():
        for x in re.findall(r"profile\." + W, f["expr"]):
            if x not in pf:
                r.error("features", f"P4 特征 {name} 引用未声明的档案字段 {x}")
        for v in f.get("view", []):
            if v not in items:
                r.error("features", f"P4 特征 {name} 引用未声明的视图项 {v}")
        for fld in re.findall(r"fact\." + W, f["expr"]):
            if not fact_field_ok(fld, fact_types):
                r.error("features", f"P4 特征 {name} 引用未声明的事实字段 {fld}")

    for tf in pol["templates"]:
        where = str((pack_dir / tf).relative_to(repo.root))
        text = (pack_dir / tf).read_text(encoding="utf-8")
        doc = load_yaml(pack_dir / tf)
        for e in sorted(_template_validator(repo).iter_errors(doc), key=lambda e: list(e.absolute_path)):
            r.error(str((pack_dir / tf).relative_to(repo.root)), f"P0 /{'/'.join(map(str, e.absolute_path))}：{e.message}")
        t = doc["template"]
        fts = t["fact_types"]
        for x in fts:
            if x not in fact_types:
                r.error(where, f"P4 事实类型 {x} 未在视图目录中声明")
        if not set(t["scope"]["fact_types"]) <= set(fts):
            r.error(where, "P4 scope 的事实类型超出模板允许范围")
        for v in t.get("view", []):
            if v not in items:
                r.error(where, f"P4 视图项 {v} 未声明")
        for pn, pd in t.get("params", {}).items():
            df = pd.get("default_from", "")
            for x in re.findall(r"profile\." + W, df):
                if x not in pf:
                    r.error(where, f"P4 参数 {pn} 的 default_from 引用未声明的档案字段 {x}")
        ids = set()
        for clause, prefix in CLAUSES.items():
            for rule in t.get(clause) or []:
                rid = rule["id"]
                if not rid.startswith(prefix):
                    r.error(where, f"P5 规则 {rid} 前缀应为 {prefix}")
                if rid in ids:
                    r.error(where, f"P5 规则编号 {rid} 重复")
                ids.add(rid)
                if clause == "exclude" and "reason" not in rule:
                    r.error(where, f"P5 排除规则 {rid} 缺少 reason")
                if "reason" in rule and rule["reason"] not in reasons:
                    r.error(where, f"P4 规则 {rid} 的原因代码 {rule['reason']} 未登记")
                expr = rule["when"] + " " + rule.get("say", "")
                for x in re.findall(r"profile\." + W, expr):
                    if x not in pf:
                        r.error(where, f"P4 规则 {rid} 引用未声明的档案字段 {x}")
                for pn in re.findall(r"params\." + W, expr):
                    if pn not in t.get("params", {}):
                        r.error(where, f"P4 规则 {rid} 引用未声明的参数 {pn}")
                for fn in re.findall(r"features\." + W, rule["when"]):
                    if fn not in feats:
                        r.error(where, f"P4 规则 {rid} 引用未声明的特征 {fn}")
                    else:
                        for v in feats[fn].get("view", []):
                            if v not in t.get("view", []):
                                r.error(where, f"P4 规则 {rid} 使用的特征 {fn} 需要视图项 {v}")
                for fld in re.findall(r"fact\." + W, rule["when"]):
                    if not fact_field_ok(fld, fts):
                        r.error(where, f"P4 规则 {rid} 引用未声明的事实字段 {fld}")
        for ev in set(EVENT_LITERAL.findall(text)):
            if ev not in events:
                r.error(where, f"P6 事件类型 {ev} 未在 contracts/events 中定义")
        for ex in t.get("examples", []):
            if ex.get("expect") not in DISPOSITIONS:
                r.error(where, f"P7 样例「{ex.get('name')}」的期望处置 {ex.get('expect')!r} 不合法")
    return r


def check_packs(repo: Repo) -> Report:
    r = Report()
    n = 0
    for pack_file in sorted((repo.root / "packs").glob("*/*/pack.yaml")):
        kind = load_yaml(pack_file)["pack"]["kind"]
        n += 1
        if kind == "information_domain":
            r.extend(check_information_pack(repo, pack_file.parent))
        elif kind == "application_domain":
            r.extend(check_application_pack(repo, pack_file.parent))
    r.note(f"扩展包一致性检查：{n} 个包")
    return r
