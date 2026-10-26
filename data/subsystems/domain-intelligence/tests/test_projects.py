"""冻结配对采购聚合：复用现有真实PostgreSQL plane，不另建测试基础设施。"""
import copy
import dataclasses
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

import pytest
from sqlalchemy import text

from miaoqia import domain_intelligence
from miaoqia.domain_intelligence.api import Basis
from miaoqia.domain_intelligence.application import projects
from miaoqia.evidence.replay import replay
from miaoqia.kernel import payloads, reqctx

from test_facts import CORPUS, _deliver, _events, _facts, plane  # noqa: F401


def _projects(plane):
    with reqctx.use(dataclasses.replace(reqctx.service("test"), purpose="object_page")):
        return domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True).query_facts(
            "INF-PROC", projects.PROJECT_TYPE, Basis("current")).items


def _revisions(plane):
    replay(plane[0], plane[2], [CORPUS / f"official-building-{kind}-20261026" for kind in ("tender", "award")])
    return [plane[3].extractions.revision(e["data"]["revision_ref"])
            for e in _events(plane[0], "evidence.extraction.revised.v1")]


def _materialize(plane, revision):
    with reqctx.use(reqctx.service("domain-intelligence")), plane[1].tx() as conn:
        return plane[3].materialize(conn, revision)


@pytest.mark.parametrize("reverse", [False, True])
def test_pair_links_without_lots_and_is_replay_idempotent(plane, reverse):
    revisions = _revisions(plane)
    if reverse:
        revisions.reverse()
    _materialize(plane, revisions[0])
    cutoff = datetime.now(timezone.utc)
    first = _projects(plane)[0]
    _materialize(plane, revisions[1])
    (project,) = _projects(plane)
    notices = _facts(plane[1])
    assert project["stage"] == "awarded" and project["lots"] == [] and project["version"] == 2
    assert set(project["notice_ids"]) == {n["id"] for n in notices}
    assert project["project_code"]["value"] == "XCXCG-2025-008"
    assert len(project["rights_tags"]) == 3
    assert all("project_id" not in n for n in notices)  # 不将当前关联写入固定公告
    events = sum((_events(plane[1], f"tendering.{name}.v1") for name in
                  ("notice.published", "project.linked", "project.stage_changed", "result.published")), [])
    expected = 6 if reverse else 7  # award先到时，后到tender不再改变阶段
    assert len(events) == expected
    by_id = {n["id"]: n for n in notices}
    for event in events:
        payloads.validator(event["type"], {}).validate(event["data"])
        assert event["data"]["fact_version"] == by_id[event["data"]["notice_id"]]["version"]
    (result,) = _events(plane[1], "tendering.result.published.v1")
    assert result["data"]["lot_ids"] == []
    for revision in revisions * 2:
        assert _materialize(plane, revision) == []
    assert _projects(plane) == (project,)
    assert len(_events(plane[1], "tendering.project.linked.v1")) == 2
    ctx = dataclasses.replace(reqctx.service("test"), purpose="object_page")
    with reqctx.use(ctx):
        reader = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
        assert reader.get_fact(project["id"], Basis("known_at", cutoff)).fact == first
        assert reader.get_fact(project["id"], Basis("current"), version=1).fact == first
        # 任一依赖权利撤销，当前聚合不可见；不能选触发修订的较宽标签。
        denied = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose:
                                            all(t != "corpus:official-building-tender-20261026" for t in tags))
        assert denied.get_fact(project["id"], Basis("current")) is None
        assert denied.query_facts("INF-PROC", projects.PROJECT_TYPE, Basis("current")).items == ()


@pytest.mark.parametrize("kind,expected", [("buyer", "confirmed"), ("missing_code", "pending_project_code"),
    ("missing_buyer", "pending_buyer"), ("conflict", "pending_conflicting_code"), ("namespace", "pending_namespace")])
def test_missing_and_conflicting_identity_never_merges(plane, kind, expected):
    tender, award = _revisions(plane)
    _materialize(plane, tender)
    facts = copy.deepcopy(list(award.facts))
    source = award.source_id
    if kind == "buyer":
        facts[0]["buyer"]["name_raw"] = "另一采购方"
    elif kind == "missing_code":
        facts[0].pop("project_code")
    elif kind == "missing_buyer":
        facts[0].pop("buyer")
    elif kind == "namespace":
        source = "unregistered-source"
    else:
        facts[0]["lots"][0]["project_code"] = {**facts[0]["project_code"], "value": "DIFFERENT-CODE"}
    notice = _materialize(plane, dataclasses.replace(award, facts=tuple(facts), source_id=source))[0]
    projects_ = _projects(plane)
    assert len(projects_) == (2 if kind == "buyer" else 1)
    assert all(len(p["notice_ids"]) == 1 and p["lots"] == [] for p in projects_)
    with reqctx.use(reqctx.service("test")), plane[1].tx() as conn:
        status = conn.execute(text("SELECT status FROM dom_notice_links WHERE notice_id=:n"), {"n": notice["id"]}).scalar_one()
    assert status == expected


def test_changed_notice_revision_keeps_old_aggregation_and_records_pending(plane):
    tender, award = _revisions(plane)
    first = _materialize(plane, tender)[0]
    _materialize(plane, award)
    project = _projects(plane)[0]
    facts = copy.deepcopy(list(tender.facts))
    facts[0]["project_code"]["value"] = "OTHER-CODE"
    changed = dataclasses.replace(tender, id="exr_" + "1" * 26, facts=tuple(facts))
    second = _materialize(plane, changed)[0]
    assert second["id"] == first["id"] and second["version"] == 2
    assert second["revision_ref"] == changed.id
    assert _projects(plane) == (project,)
    assert len(_events(plane[1], "tendering.notice.published.v1")) == 2
    with reqctx.use(reqctx.service("test")), plane[1].tx() as conn:
        assert conn.execute(text("SELECT status FROM dom_notice_links WHERE notice_id=:n AND notice_version=2"),
                            {"n": first["id"]}).scalar_one() == "pending_revision"
        assert conn.execute(text("SELECT notice_version FROM dom_project_dependencies WHERE project_id=:p AND project_version=2 AND notice_id=:n"),
                            {"p": project["id"], "n": first["id"]}).scalar_one() == 1


def test_concurrent_pair_and_duplicate_deliveries_have_one_project(plane):
    revisions = _revisions(plane)
    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(lambda r: _materialize(plane, r), revisions * 2))
    (project,) = _projects(plane)
    assert project["version"] == 2 and project["stage"] == "awarded"
    assert len(_events(plane[1], "tendering.notice.published.v1")) == 2
    assert len(_events(plane[1], "tendering.project.linked.v1")) == 2
    assert len(_events(plane[1], "tendering.result.published.v1")) == 1


def test_tendering_payload_failure_rolls_back_notice_project_links_and_outbox(plane, monkeypatch):
    revision = _revisions(plane)[0]
    actual_emit = projects._emit
    def fail(conn, name, notice, project_id, rights_tags, **extra):
        extra["to"] = None  # 真实契约校验拒绝阶段值；此时事实/依赖已经写入同一事务。
        return actual_emit(conn, name, notice, project_id, rights_tags, **extra)
    monkeypatch.setattr(projects, "_emit", fail)
    with pytest.raises(Exception, match="None is not of type"):
        _materialize(plane, revision)
    assert _facts(plane[1]) == [] and _projects(plane) == ()
    with reqctx.use(reqctx.service("test")), plane[1].tx() as conn:
        for table in ("kernel_outbox", "dom_notice_links", "dom_project_keys", "dom_project_dependencies", "dom_fact_versions"):
            assert conn.execute(text(f"SELECT count(*) FROM {table}")).scalar_one() == 0


def test_existing_0001_notice_upgrades_with_base_derivation_and_remains_immutable(plane, pg_factory):
    from alembic import command
    from alembic.config import Config
    from miaoqia.domain_intelligence import MIGRATIONS
    from miaoqia.kernel import db
    import json
    import psycopg

    notice = _materialize(plane, _revisions(plane)[0])[0]
    dsns = pg_factory()
    db.migrate(dsns["owner"])
    cfg = Config()
    cfg.set_main_option("script_location", MIGRATIONS)
    cfg.set_main_option("sqlalchemy.url", db._url(dsns["owner"]))
    command.upgrade(cfg, "0001")
    with psycopg.connect(dsns["app"]) as conn:
        conn.execute("INSERT INTO dom_facts VALUES (%s,'INF-PROC','tendering.notice','old-source','old-key',0,1,now(),now())", (notice["id"],))
        conn.execute("""INSERT INTO dom_fact_versions VALUES (%s,1,%s,%s,%s,%s,%s,%s,%s,%s,%s)""",
                     (notice["id"], json.dumps(notice), "sha256:" + "0" * 64, notice["schema_version"],
                      notice["snapshot_ref"], notice["revision_ref"], '["/title"]', json.dumps(notice["rights_tags"]),
                      notice["observed_at"], notice["recorded_at"]))
    command.upgrade(cfg, "head")
    with psycopg.connect(dsns["app"]) as conn:
        row = conn.execute("SELECT content, derivation_key FROM dom_fact_versions").fetchone()
        assert row == (notice, "base")
    with psycopg.connect(dsns["app"]) as conn, pytest.raises(psycopg.errors.IntegrityConstraintViolation):
        conn.execute("UPDATE dom_fact_versions SET content='{}'")
    # 已运行聚合的库拒绝回退，不能丢弃关联与版本依据。
    cfg.set_main_option("sqlalchemy.url", db._url(plane[4]["owner"]))
    with pytest.raises(Exception, match="拒绝有损回退"):
        command.downgrade(cfg, "0001")
    assert len(_projects(plane)) == 1


@pytest.mark.parametrize("change", ["uscc", "alias", "hyphen", "case"])
def test_identity_does_not_guess_buyer_alias_or_number_normalization(plane, change):
    tender, award = _revisions(plane)
    tf, af = copy.deepcopy(list(tender.facts)), copy.deepcopy(list(award.facts))
    if change == "uscc":
        # 同原名但明确异体不合并；不以同名覆盖显式身份。
        tf[0]["buyer"]["uscc"] = "91321283MADWT8A11R"
        af[0]["buyer"]["uscc"] = "91321283MADWT8A11X"
    elif change == "alias":
        af[0]["buyer"]["name_raw"] = "江山水务"
    elif change == "hyphen":
        af[0]["project_code"]["value"] = "XCXCG2025008"
    else:
        af[0]["project_code"]["value"] = "xcxcg-2025-008"
    _materialize(plane, dataclasses.replace(tender, facts=tuple(tf)))
    _materialize(plane, dataclasses.replace(award, facts=tuple(af)))
    assert len(_projects(plane)) == 2
