import json
from pathlib import Path

import yaml

from mqc.compat import bump_ok, compat, diff
from mqc.repo import Repo

NOTICE = "contracts/schemas/domains/procurement/notice.schema.json"
PDEFS = "contracts/schemas/domains/procurement/defs.schema.json"


def edit_json(root: Path, rel: str, fn) -> None:
    p = root / rel
    d = json.loads(p.read_text(encoding="utf-8"))
    fn(d)
    p.write_text(json.dumps(d, ensure_ascii=False, indent=2), encoding="utf-8")


def edit_text(root: Path, rel: str, old: str, new: str) -> None:
    p = root / rel
    s = p.read_text(encoding="utf-8")
    assert old in s
    p.write_text(s.replace(old, new, 1), encoding="utf-8")


def bump_version(root: Path, rel: str, key: str) -> None:
    """把文件中 key 下的 version 提升一个次版本（与当前取值无关）。"""
    p = root / rel
    cur = yaml.safe_load(p.read_text(encoding="utf-8"))[key]["version"]
    major, minor, _ = (int(x) for x in cur.split("."))
    edit_text(root, rel, f"version: {cur}", f"version: {major}.{minor + 1}.0")


def run(root: Path):
    return compat(Repo(root), "HEAD", verbose=True)


def levels(changes):
    return {c.level for c in changes}


# ---------- 判定规则 ----------

def test_diff_rules():
    base = {"type": "object", "required": ["a"], "properties": {"a": {"type": "string"}, "b": {"enum": ["x", "y"]}},
            "additionalProperties": False}
    assert diff(base, base, "T") == []
    assert levels(diff(base, {**base, "description": "说明"}, "T")) == {"doc"}
    rm = {**base, "properties": {"a": {"type": "string"}}}
    assert levels(diff(base, rm, "T")) == {"breaking"}
    add = {**base, "properties": {**base["properties"], "c": {"type": "string"}}}
    assert levels(diff(base, add, "T")) == {"additive"}
    req = {**base, "required": ["a", "b"]}
    assert levels(diff(base, req, "T")) == {"breaking"}
    enum_add = {**base, "properties": {**base["properties"], "b": {"enum": ["x", "y", "z"]}}}
    assert levels(diff(base, enum_add, "T")) == {"additive"}
    enum_rm = {**base, "properties": {**base["properties"], "b": {"enum": ["x"]}}}
    assert levels(diff(base, enum_rm, "T")) == {"breaking"}
    tighter = {"type": "string", "maxLength": 10}
    assert levels(diff({"type": "string", "maxLength": 20}, tighter, "T")) == {"breaking"}
    assert levels(diff(tighter, {"type": "string", "maxLength": 20}, "T")) == {"additive"}
    assert levels(diff({"type": "string"}, {"type": "string", "pattern": "^a"}, "T")) == {"breaking"}
    assert levels(diff({"allOf": [{"if": {}, "then": {"required": ["a"]}}]},
                       {"allOf": [{"if": {}, "then": {"required": ["b"]}}]}, "T")) == {"breaking"}


def test_bump_rules():
    assert bump_ok("0.1.0", "0.2.0", "breaking")       # 0.x：不兼容变更升次版本即可
    assert not bump_ok("0.1.0", "0.1.1", "breaking")
    assert bump_ok("1.2.0", "2.0.0", "breaking")
    assert not bump_ok("1.2.0", "1.3.0", "breaking")   # 1.0 起不兼容变更须升主版本
    assert bump_ok("1.2.0", "1.3.0", "additive")
    assert not bump_ok("1.2.0", "1.2.1", "additive")
    assert bump_ok("1.2.0", "1.2.0", "doc")


# ---------- 端到端：Schema ----------

def test_unchanged_passes(sandbox):
    assert run(sandbox).errors == []


def test_removed_field_requires_bump_on_file_and_roots(sandbox):
    # 删除共用定义中的字段：定义文件与引用它的顶层 Schema 都必须升版本
    edit_json(sandbox, PDEFS, lambda d: d["$defs"]["item"]["properties"].pop("spec"))
    errs = run(sandbox).errors
    assert any("procurement/defs.schema.json" in e and "breaking" in e for e in errs)
    assert any("procurement/notice.schema.json" in e for e in errs)
    assert any("procurement-project.schema.json" in e for e in errs)


def test_bump_satisfies(sandbox):
    edit_json(sandbox, PDEFS, lambda d: d["$defs"]["item"]["properties"].pop("spec"))
    for rel in (PDEFS, NOTICE, "contracts/schemas/domains/procurement/procurement-project.schema.json"):
        edit_json(sandbox, rel, lambda d: d.update({"x-version": "0.2.0"}))
    assert run(sandbox).errors == []


def test_additive_requires_minor(sandbox):
    edit_json(sandbox, NOTICE, lambda d: d["properties"].update({"source_url": {"type": "string"}}))
    errs = run(sandbox).errors
    assert any("notice.schema.json" in e and "additive" in e for e in errs)
    edit_json(sandbox, NOTICE, lambda d: d.update({"x-version": "0.2.0"}))
    assert run(sandbox).errors == []


def test_doc_change_needs_no_bump(sandbox):
    edit_json(sandbox, NOTICE, lambda d: d.update({"description": d["description"] + "（补充说明）"}))
    assert run(sandbox).errors == []


# ---------- 端到端：事件、前缀、扩展包 ----------

def test_event_breaking_change_rejected_even_with_bump(sandbox):
    edit_text(sandbox, "contracts/events/tendering/events.asyncapi.yaml",
              "required: [method, confidence, status]", "required: [method, confidence, status, reviewer]")
    errs = run(sandbox).errors
    assert any("tendering.project.linked.v1" in e and ".v<n+1>" in e for e in errs)


def test_event_removed_rejected(sandbox):
    p = sandbox / "contracts/events/credit/events.asyncapi.yaml"
    d = yaml.safe_load(p.read_text(encoding="utf-8"))
    d["components"]["messages"].pop("recordResolved")
    d["channels"]["public.domain"]["messages"].pop("recordResolved")
    p.write_text(yaml.safe_dump(d, allow_unicode=True, sort_keys=False), encoding="utf-8")
    assert any("credit.record.resolved.v1" in e for e in run(sandbox).errors)


def test_prefix_removed_rejected(sandbox):
    edit_text(sandbox, "contracts/identifiers.yaml", "  rel: { object: Relationship, owner: entity-graph }\n", "")
    assert any("rel_" in e for e in run(sandbox).errors)


def test_template_change_requires_versions(sandbox):
    tpl = "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml"
    edit_text(sandbox, tpl, "default: 7, min: 1", "default: 10, min: 1")
    errs = run(sandbox).errors
    assert any("pack.version" in e for e in errs)
    assert any("template.version" in e for e in errs)
    bump_version(sandbox, tpl, "template")
    bump_version(sandbox, "packs/application_domains/tendering/pack.yaml", "pack")
    assert run(sandbox).errors == []
