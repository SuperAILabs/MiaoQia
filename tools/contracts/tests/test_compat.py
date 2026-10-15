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


def bump_schema(root: Path, rel: str, *, breaking: bool = False) -> None:
    """按兼容级别提升版本：稳定契约的破坏性变更须升主版本。"""
    def f(d):
        major, minor, _ = (int(x) for x in d["x-version"].split("."))
        d["x-version"] = f"{major + 1}.0.0" if breaking and major else f"{major}.{minor + 1}.0"
    edit_json(root, rel, f)


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
        bump_schema(sandbox, rel, breaking=True)
    assert run(sandbox).errors == []


def test_additive_requires_minor(sandbox):
    edit_json(sandbox, NOTICE, lambda d: d["properties"].update({"source_url": {"type": "string"}}))
    errs = run(sandbox).errors
    assert any("notice.schema.json" in e and "additive" in e for e in errs)
    bump_schema(sandbox, NOTICE)
    assert run(sandbox).errors == []


def test_doc_change_needs_no_bump(sandbox):
    edit_json(sandbox, NOTICE, lambda d: d.update({"description": d["description"] + "（补充说明）"}))
    assert run(sandbox).errors == []


# ---------- 端到端：事件、前缀、扩展包 ----------

def test_event_breaking_change_rejected_even_with_bump(sandbox):
    p = sandbox / "contracts/events/tendering/events.asyncapi.yaml"
    doc = yaml.safe_load(p.read_text())
    doc["components"]["messages"]["projectLinked"]["payload"]["allOf"][1]["required"].append("reviewer")
    p.write_text(yaml.safe_dump(doc, allow_unicode=True))
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


def test_ignored_files_do_not_count_as_pack_changes(sandbox):
    (sandbox / ".gitignore").write_text("__pycache__/\n", encoding="utf-8")
    cache = sandbox / "packs/information_domains/procurement/src/__pycache__"
    cache.mkdir(parents=True, exist_ok=True)
    (cache / "x.cpython-312.pyc").write_bytes(b"\0")
    assert not run(sandbox).errors


def test_event_compat_resolves_external_schema_constraints(sandbox):
    p = sandbox / "contracts/schemas/domains/procurement/defs.schema.json"
    doc = json.loads(p.read_text())
    doc["$defs"]["notice_type"]["enum"].remove("tender")
    doc["x-version"] = "0.4.0"
    p.write_text(json.dumps(doc))
    errors = run(sandbox).errors
    assert any("tendering.notice.published.v1" in e and "删除枚举值" in e for e in errors)


def test_stable_api_breaking_requires_new_path(sandbox):
    p = sandbox / 'contracts/apis/identity/openapi.yaml'
    d = yaml.safe_load(p.read_text())
    d['components']['schemas']['CheckRequest']['required'].append('subject')
    d['info']['version'] = '2.0.0'
    p.write_text(yaml.safe_dump(d))
    assert any('新路径主版本' in e for e in run(sandbox).errors)


def test_api_component_change_cannot_hide_behind_ref(sandbox):
    p = sandbox / 'contracts/apis/identity/openapi.yaml'
    d = yaml.safe_load(p.read_text())
    d['components']['schemas']['CheckRequest']['properties']['relation']['pattern'] = '^read$'
    p.write_text(yaml.safe_dump(d))
    assert any('API breaking' in e for e in run(sandbox).errors)


def test_storage_change_requires_migration(sandbox):
    p = sandbox / 'contracts/storage/kernel.sql'
    p.write_text(p.read_text().replace('event_id      text', 'event_id      varchar(10)'))
    assert any('迁移文件' in e for e in run(sandbox).errors)


def test_api_additive_requires_version(sandbox):
    p = sandbox / 'contracts/apis/identity/openapi.yaml'
    d = yaml.safe_load(p.read_text())
    d['components']['schemas']['CheckRequest']['properties']['reason'] = {'type': 'string'}
    p.write_text(yaml.safe_dump(d))
    assert any('API additive' in e for e in run(sandbox).errors)
    d['info']['version'] = '1.1.0'
    p.write_text(yaml.safe_dump(d))
    assert run(sandbox).errors == []


def test_identifier_owner_cannot_be_reassigned(sandbox):
    p = sandbox / 'contracts/identifiers.yaml'
    d = yaml.safe_load(p.read_text())
    d['prefixes']['usr']['owner'] = 'other'
    p.write_text(yaml.safe_dump(d))
    assert any('所有者' in e for e in run(sandbox).errors)


def test_versions_cannot_regress(sandbox):
    edit_json(sandbox, NOTICE, lambda d:d.update({'x-version':'0.9.0'}))
    assert any('不得回退' in e for e in run(sandbox).errors)


def test_new_identifier_requires_minor_version(sandbox):
    p = sandbox / 'contracts/identifiers.yaml'
    d = yaml.safe_load(p.read_text())
    d['prefixes']['new'] = {'object':'NewObject','owner':'kernel'}
    p.write_text(yaml.safe_dump(d))
    assert any('新增前缀' in e for e in run(sandbox).errors)
    major, minor, _ = map(int, d['version'].split('.'))
    d['version'] = f'{major}.{minor + 1}.0'
    p.write_text(yaml.safe_dump(d))
    assert run(sandbox).errors == []


def test_engineering_profile_change_requires_major(sandbox):
    p = sandbox / 'contracts/integration.yaml'
    d = yaml.safe_load(p.read_text())
    d['context']['header'] = 'Other-Context'
    p.write_text(yaml.safe_dump(d))
    assert any('工程配置breaking' in e for e in run(sandbox).errors)
