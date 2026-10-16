"""mqc freeze-check 的回归测试：受约束文件变化须升版本，佐证材料变化只报告。"""
import mqc.cli as cli

ORG_EVENTS = "contracts/events/org/events.asyncapi.yaml"
# 版本归属对：kernel.sql 的版本由 kernel.yaml 决定（INF-PROC 的 pack.yaml 已升过版本，不能再用来验证“未升版本即失败”）
OWNED = "contracts/storage/kernel.sql"
OWNER = "contracts/storage/kernel.yaml"


def run(root, monkeypatch, capsys):
    monkeypatch.setattr(cli, "find_root", lambda start=None: root)
    code = cli.main(["freeze-check"])
    return code, capsys.readouterr()


def edit(root, rel, old, new):
    p = root / rel
    s = p.read_text(encoding="utf-8")
    assert old in s
    p.write_text(s.replace(old, new, 1), encoding="utf-8")


def test_repo_passes(sandbox, monkeypatch, capsys):
    code, out = run(sandbox, monkeypatch, capsys)
    assert code == 0, out.err


def test_frozen_change_needs_version_bump(sandbox, monkeypatch, capsys):
    edit(sandbox, ORG_EVENTS, "title: INF-ORG 领域事件", "title: INF-ORG 领域事件（改）")
    code, out = run(sandbox, monkeypatch, capsys)
    assert code == 1 and " F3 " in out.err and ORG_EVENTS in out.err
    edit(sandbox, ORG_EVENTS, "  version: 1.0.0", "  version: 1.1.0")
    code, out = run(sandbox, monkeypatch, capsys)
    assert code == 0, out.err


def test_version_owner_decides(sandbox, monkeypatch, capsys):
    p = sandbox / OWNED
    p.write_text(p.read_text(encoding="utf-8") + "\n-- 改动\n", encoding="utf-8")
    code, out = run(sandbox, monkeypatch, capsys)
    assert code == 1 and OWNED in out.err
    edit(sandbox, OWNER, "version: 1.0.0", "version: 1.1.0")
    code, out = run(sandbox, monkeypatch, capsys)
    assert code == 0, out.err


def test_supporting_artifact_only_reported(sandbox, monkeypatch, capsys):
    p = sandbox / "contracts/tests/credit.record.yaml"
    p.write_text(p.read_text(encoding="utf-8") + "\n# 改动\n", encoding="utf-8")
    code, out = run(sandbox, monkeypatch, capsys)
    assert code == 0 and "仅报告：contracts/tests/credit.record.yaml" in out.out


def test_missing_frozen_file(sandbox, monkeypatch, capsys):
    (sandbox / ORG_EVENTS).unlink()
    code, out = run(sandbox, monkeypatch, capsys)
    assert code == 1 and " F2 " in out.err


def test_record_needs_timestamp(sandbox, monkeypatch, capsys):
    edit(sandbox, "contracts/reviews/first-batch-freeze-amendment-1.yaml", "amended_at:", "noted_at:")
    code, out = run(sandbox, monkeypatch, capsys)
    assert code == 1 and " F1 " in out.err
