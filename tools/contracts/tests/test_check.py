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
    ("Y1", "packs/information_domains/procurement/lifecycle.yaml", "trigger: [contract]", "on: [contract]"),
    ("P2", "packs/information_domains/procurement/lifecycle.yaml", "to: accepted,", "to: archived,"),
    ("P0", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "weight: 15", "weight: 0"),
    ("P0", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "mode: immediate", "mode: hourly"),
    ("P4", "packs/application_domains/tendering/policy/templates/a01-direct-bid.yaml", "reason: BLOCKED_BUYER", "reason: NOT_A_CODE"),
    ("E2", "contracts/events/org/events.asyncapi.yaml", "name: org.change.recorded.v1", "name: credit.change.recorded.v1"),
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
