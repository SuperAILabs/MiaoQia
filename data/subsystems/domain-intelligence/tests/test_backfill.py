"""旧持久化形态与事务写入升级回归；历史夹具复用当前纯版本规则，不宣称独立旧运行环境。"""
import dataclasses
import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import types

from alembic import command
from alembic.config import Config
import pytest
from sqlalchemy import text

from miaoqia.domain_intelligence import MIGRATIONS
from miaoqia.domain_intelligence.application import projects
from miaoqia.domain_intelligence.application.backfill import backfill
from miaoqia.kernel import db, reqctx

from test_facts import _events, _facts, plane  # noqa: F401
from test_projects import _projects, _revisions


@pytest.fixture
def legacy_pair(plane):
    cfg = Config()
    cfg.set_main_option("script_location", MIGRATIONS)
    cfg.set_main_option("sqlalchemy.url", db._url(plane[4]["owner"]))
    command.downgrade(cfg, "0001")
    # 原样冻结自c0694cf的旧Materializer；旧库无derivation列，不可调用现writer代替。
    source = (Path(__file__).parent / "fixtures/0001_facts.py").read_bytes()
    assert hashlib.sha256(source).hexdigest() == "b48e9f40efc46081ba6eadf73ff35f396cddadb412c0b0e71d9f5e7edfed6165"
    old = types.ModuleType("legacy_0001")
    old.__package__ = "miaoqia.domain_intelligence.application"
    exec(compile(source, "legacy_0001", "exec"), old.__dict__)
    revisions = _revisions(plane)
    baseline = old.Materializer(plane[3].extractions)
    for revision in revisions:
        with reqctx.use(reqctx.service("domain-intelligence")), plane[1].tx() as conn:
            baseline.materialize(conn, revision)
    return plane, cfg, baseline, revisions


def _run_backfill(plane, ids, reader=None):
    with reqctx.use(reqctx.service("domain-intelligence")):
        return backfill(plane[1], reader or plane[3].extractions, ids)


def test_old_pair_upgrade_cli_backfill_is_idempotent_and_preserves_originals(legacy_pair):
    plane, cfg, _, _ = legacy_pair
    notices = _facts(plane[1])
    original_events = _events(plane[1], "domain.fact.upserted.v1")
    assert len(notices) == 2 and len(original_events) == 2
    command.upgrade(cfg, "head")
    env = dict(os.environ, MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN=plane[4]["app"],
               MIAOQIA_EVIDENCE_DB_DSN=plane[0].engine.url.render_as_string(hide_password=False).replace("postgresql+psycopg:", "postgresql:"))
    # 运行实际安装的console entrypoint，绕过消费inbox去重。
    entry = Path(sys.executable).parent / "mq-project-backfill"
    args = [str(entry), *(n["id"] for n in notices)]
    first = subprocess.run(args, env=env, capture_output=True, text=True)
    assert first.returncode == 0, first.stderr
    assert {r["action"] for r in json.loads(first.stdout)} == {"processed"}
    (project,) = _projects(plane)
    assert project["stage"] == "awarded" and project["lots"] == [] and len(project["notice_ids"]) == 2
    assert _facts(plane[1]) == notices
    assert _events(plane[1], "domain.fact.upserted.v1")[:2] == original_events
    all_events = _events(plane[1], "tendering.notice.published.v1")
    assert len(all_events) == 2 and len(_events(plane[1], "tendering.project.linked.v1")) == 2
    assert len(_events(plane[1], "tendering.result.published.v1")) == 1
    with reqctx.use(reqctx.service("test")), plane[1].tx() as conn:
        before = conn.execute(text("SELECT envelope FROM kernel_outbox ORDER BY seq")).scalars().all()
    second = subprocess.run(args, env=env, capture_output=True, text=True)
    assert second.returncode == 0, second.stderr
    assert {r["action"] for r in json.loads(second.stdout)} == {"unchanged"}
    assert _projects(plane) == (project,) and _facts(plane[1]) == notices
    with reqctx.use(reqctx.service("test")), plane[1].tx() as conn:
        assert conn.execute(text("SELECT envelope FROM kernel_outbox ORDER BY seq")).scalars().all() == before
    bad_env = {k: v for k, v in env.items() if k != "MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN"}
    missing_config = subprocess.run(args, env=bad_env, capture_output=True, text=True)
    assert missing_config.returncode == 2 and "未配置" in missing_config.stderr


def test_backfill_rejects_invalid_targets_and_revision_metadata_atomically(legacy_pair):
    plane, cfg, _, revisions = legacy_pair
    command.upgrade(cfg, "head")
    ids = [n["id"] for n in _facts(plane[1])]
    for invalid in ([], ["pp_" + "Z" * 26], ["ntc_" + "Z" * 26], ids * 51):
        with pytest.raises(ValueError):
            _run_backfill(plane, invalid)
    for field, value in (("source_id", "other"), ("source_key", "other"), ("snapshot_ref", "snp_" + "Z" * 26),
                         ("rights_tags", ("public-official-review",))):
        class Wrong:
            def revision(self, ref):
                actual = plane[3].extractions.revision(ref)
                return dataclasses.replace(actual, **{field: value})
        with pytest.raises(ValueError, match="不一致"):
            _run_backfill(plane, ids, Wrong())
    class WrongEvidence:
        def revision(self, ref):
            actual = plane[3].extractions.revision(ref)
            facts = copy.deepcopy(list(actual.facts))
            facts[0]["title"]["prov"]["confidence"] = 0.1
            return dataclasses.replace(actual, facts=tuple(facts))
    with pytest.raises(ValueError, match="字段/证据"):
        _run_backfill(plane, ids, WrongEvidence())
    assert _projects(plane) == () and _events(plane[1], "tendering.notice.published.v1") == []


def test_backfill_payload_fault_rolls_back_every_new_write(legacy_pair, monkeypatch):
    plane, cfg, _, _ = legacy_pair
    command.upgrade(cfg, "head")
    notices, original = _facts(plane[1]), _events(plane[1], "domain.fact.upserted.v1")
    actual = projects._emit
    def invalid(conn, name, notice, project_id, rights_tags, **extra):
        if name == "project.stage_changed":
            extra["to"] = None
        return actual(conn, name, notice, project_id, rights_tags, **extra)
    monkeypatch.setattr(projects, "_emit", invalid)
    with pytest.raises(Exception, match="None is not of type"):
        _run_backfill(plane, [n["id"] for n in notices])
    assert _facts(plane[1]) == notices and _events(plane[1], "domain.fact.upserted.v1") == original
    assert _projects(plane) == ()
    with reqctx.use(reqctx.service("test")), plane[1].tx() as conn:
        for table in ("dom_notice_links", "dom_project_keys", "dom_project_dependencies"):
            assert conn.execute(text(f"SELECT count(*) FROM {table}")).scalar_one() == 0


def test_backfill_old_revision_gt_one_stays_pending(legacy_pair):
    plane, cfg, baseline, revisions = legacy_pair
    from miaoqia.evidence.replay import corpus_artifact
    from test_facts import CORPUS
    # 使用真实证据管线产生后续修订，仍由旧writer写入0001。
    artifact = corpus_artifact(CORPUS / "official-building-tender-20261026")
    changed = dataclasses.replace(artifact, raw=artifact.raw.replace(b"2026", b"2027"))
    with reqctx.use(reqctx.service("evidence")), plane[0].tx() as conn:
        plane[2].ingest(conn, changed)
    ref = _events(plane[0], "evidence.extraction.revised.v1")[-1]["data"]["revision_ref"]
    with reqctx.use(reqctx.service("domain-intelligence")), plane[1].tx() as conn:
        baseline.materialize(conn, plane[3].extractions.revision(ref))
    command.upgrade(cfg, "head")
    notices = _facts(plane[1])
    assert sorted(n["version"] for n in notices) == [1, 2]
    results = _run_backfill(plane, [n["id"] for n in notices])
    assert {r["status"] for r in results} == {"confirmed", "pending_revision"}
    assert len(_projects(plane)[0]["notice_ids"]) == 1
    assert _facts(plane[1]) == notices


def test_backfill_unregistered_source_reports_pending_without_project(legacy_pair):
    plane, cfg, baseline, _ = legacy_pair
    from miaoqia.evidence.replay import replay
    from test_facts import CORPUS
    replay(plane[0], plane[2], [CORPUS / "official-award-20261014"])
    ref = _events(plane[0], "evidence.extraction.revised.v1")[-1]["data"]["revision_ref"]
    with reqctx.use(reqctx.service("domain-intelligence")), plane[1].tx() as conn:
        notice = baseline.materialize(conn, plane[3].extractions.revision(ref))[0]
    command.upgrade(cfg, "head")
    (result,) = _run_backfill(plane, [notice["id"]])
    assert result["status"] == "pending_namespace" and result["project_id"] is None
    assert _projects(plane) == ()
    assert _run_backfill(plane, [notice["id"]])[0]["action"] == "unchanged"
