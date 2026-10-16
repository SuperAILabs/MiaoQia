"""mqc check 的回归测试：向仓库副本注入各类错误，确认都被检出。"""
import json

import pytest

import mqc.cli as cli

CREDIT = "contracts/schemas/domains/credit/record.schema.json"


def run_check(root, monkeypatch, capsys):
    monkeypatch.setattr(cli, "find_root", lambda start=None: root)
    code = cli.main(["check"])
    return code, capsys.readouterr()


def test_repo_passes(sandbox, monkeypatch, capsys):
    code, _ = run_check(sandbox, monkeypatch, capsys)
    assert code == 0


@pytest.mark.parametrize("rule,mutate", [
    ("S3", lambda d: d["properties"].update({"x": {"type": ["string", "null"]}})),
    ("S5", lambda d: d["properties"].update({"x": {"type": "string", "pattern": "^qqq_[0-9A-HJKMNP-TV-Z]{26}$"}})),
    ("S6", lambda d: d["properties"]["record_type"]["enum"].append("Bad-Value")),
    ("S7", lambda d: d["properties"].update({"x": {"$ref": "../../common/types.schema.json#/$defs/nope"}})),
    ("S9", lambda d: d.pop("x-version")),
])
def test_schema_rules(sandbox, monkeypatch, capsys, rule, mutate):
    p = sandbox / CREDIT
    d = json.loads(p.read_text(encoding="utf-8"))
    mutate(d)
    p.write_text(json.dumps(d, ensure_ascii=False), encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and f" {rule} " in out.err


@pytest.mark.parametrize("rule,rel,old,new", [
    ("Y1", "packs/information_domains/procurement/lifecycle.yaml", "trigger:\n    - contract", "on:\n    - contract"),
    ("P2", "packs/information_domains/procurement/lifecycle.yaml", "to: accepted", "to: archived"),
    ("P0", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "weight: 15", "weight: 0"),
    ("P0", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "mode: immediate", "mode: hourly"),
    ("P4", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "reason: BLOCKED_BUYER", "reason: NOT_A_CODE"),
    ("P6", "packs/application_domains/tendering/policy/views.yaml", "- tendering.deadline.changed.v1", "- tendering.deadline.moved.v1"),
    ("E2", "contracts/events/org/events.asyncapi.yaml", "name: org.change.recorded.v1", "name: credit.change.recorded.v1"),
    ("P8", "packs/information_domains/procurement/relationships.yaml", "to_path: lots[*].bid_results[*].awarded_items[*].brand", "to_path: lots[*].bid_results[*].awarded_items[*].brand_required"),
    ("P8", "packs/information_domains/procurement/relationships.yaml", "    from: brand\n", "    from: lot\n"),
    ("P8", "packs/information_domains/procurement/relationships.yaml", "qualifiers: [domain_fact, lot]\n    validity: event\n    sources:\n      - schema: contracts/schemas/domains/procurement/notice.schema.json\n        from_path: project_id", "qualifiers: [domain_fact]\n    validity: event\n    sources:\n      - schema: contracts/schemas/domains/procurement/notice.schema.json\n        from_path: project_id"),
    ("P8", "packs/information_domains/investment_projects/relationships.yaml", "- type: constructed_by", "- type: designed_by"),
])
def test_text_rules(sandbox, monkeypatch, capsys, rule, rel, old, new):
    p = sandbox / rel
    s = p.read_text(encoding="utf-8")
    assert old in s
    p.write_text(s.replace(old, new, 1), encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and f" {rule} " in out.err


def test_bad_fixture_detected(sandbox, monkeypatch, capsys):
    p = sandbox / "contracts/tests/credit.record.yaml"
    s = p.read_text(encoding="utf-8")
    p.write_text(s.replace("  - name: 失信缺执行法院\n    valid: false", "  - name: 失信缺执行法院\n    valid: true", 1), encoding="utf-8")
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and "应通过，实际被拒绝" in out.err


def test_missing_event_fixture_detected(sandbox, monkeypatch, capsys):
    (sandbox / "contracts/events/entity/fixtures.yaml").unlink()
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and "E5" in out.err and "缺少事件载荷正反例" in out.err


def test_datetime_format_is_active():
    from jsonschema import Draft202012Validator
    v = Draft202012Validator({'type': 'string', 'format': 'date-time'},
                             format_checker=Draft202012Validator.FORMAT_CHECKER)
    assert v.is_valid('2026-10-14T08:00:00Z')
    assert not v.is_valid('not-a-time')
    assert not v.is_valid('2026-02-30T08:00:00Z')


def test_bad_api_fixture_detected(sandbox, monkeypatch, capsys):
    import yaml
    p = sandbox / 'contracts/apis/identity/fixtures.yaml'
    d = yaml.safe_load(p.read_text())
    d['fixtures'][0]['base']['tenant'] = 'other'
    p.write_text(yaml.safe_dump(d))
    code, out = run_check(sandbox, monkeypatch, capsys)
    assert code == 1 and '应通过，实际被拒绝' in out.err


def test_replace_missing_fixture_path_is_not_a_negative_control():
    from mqc.fixtures import _apply
    with pytest.raises(ValueError, match='目标不存在'):
        _apply({}, {'op':'replace','path':'/missing','value':1})
