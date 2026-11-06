"""领域事实物化与 A02 结果视图投影：真实 PostgreSQL；含越权与反例。"""
from __future__ import annotations

import copy
import dataclasses
import json
from pathlib import Path

import psycopg
import pytest
from sqlalchemy import text

from miaoqia import evidence
from miaoqia.domain_intelligence import MIGRATIONS as DOM_MIGRATIONS
from miaoqia.domain_intelligence.application.facts import OBJECT_TYPES, Materializer, fact_version
from miaoqia.domain_intelligence.domain.views import bid_result_units
from miaoqia.evidence import MIGRATIONS as EVD_MIGRATIONS
from miaoqia.evidence.application.pipeline import Pipeline
from miaoqia.evidence.extensions import packs
from miaoqia.evidence.infrastructure.objectstore import FilesystemObjectStore
from miaoqia.evidence.replay import replay
from miaoqia.kernel import db, payloads, reqctx

REPO = Path(__file__).resolve().parents[4]
CORPUS = REPO / "packs/information_domains/procurement/corpus"


@pytest.fixture
def plane(pg_factory, tmp_path):
    evd, dom = pg_factory(), pg_factory()
    db.migrate(evd["owner"], EVD_MIGRATIONS)
    db.migrate(dom["owner"], DOM_MIGRATIONS)
    evd_db, dom_db = db.Database(evd["app"]), db.Database(dom["app"])
    pipeline = Pipeline(FilesystemObjectStore(tmp_path / "objects"), packs.load(REPO / "packs"))
    reader = evidence.extractions(evd["app"])
    yield evd_db, dom_db, pipeline, Materializer(reader), dom
    reader.close()
    evd_db.close()
    dom_db.close()


def _events(database, type_):
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        rows = conn.execute(text("SELECT envelope FROM kernel_outbox ORDER BY seq")).scalars().all()
    return [e for e in rows if e["type"] == type_]


def _deliver(plane, samples):
    """投放语料，并把 evidence 发件箱中的解析修订事件逐条交给物化处理（等同消费框架在事务内调用处理器）。"""
    evd_db, dom_db, pipeline, materializer, _ = plane
    replay(evd_db, pipeline, samples)
    for env in _events(evd_db, "evidence.extraction.revised.v1"):
        with reqctx.use(reqctx.service("domain-intelligence")), dom_db.tx() as conn:
            materializer.on_extraction_revised(conn, env)


def _facts(dom_db):
    with reqctx.use(reqctx.service("test")), dom_db.tx() as conn:
        ids = conn.execute(text("SELECT id FROM dom_facts WHERE object_type = 'tendering.notice' ORDER BY created_at, id")).scalars().all()
        return [fact_version(conn, i) for i in ids]


def test_schema_versions_match_the_contracts():
    schemas = {"INF-PROC": "procurement/notice", "INF-ORG": "organization/registration"}
    for domain, (_, _, _, version) in OBJECT_TYPES.items():
        schema = json.loads((REPO / f"contracts/schemas/domains/{schemas[domain]}.schema.json").read_text())
        assert schema["x-version"] == version


def test_replayed_award_becomes_a_fact_version_with_a_valid_event(plane):
    _deliver(plane, [CORPUS / "official-award-20261014"])
    dom_db = plane[1]
    (fact,) = _facts(dom_db)
    assert fact["id"].startswith("ntc_") and fact["version"] == 1 and fact["classification"] == "public"
    assert fact["rights_tags"] == ["public-official-review", "corpus:official-award-20261014"]
    (event,) = _events(dom_db, "domain.fact.upserted.v1")
    assert event["miaoqiazone"] == "public" and "miaoqiatenant" not in event
    assert not list(payloads.validator(event["type"], {}).iter_errors(event["data"]))
    assert event["data"]["fact_id"] == fact["id"] and event["data"]["object_type"] == "tendering.notice"
    assert "/lots" in event["data"]["changed_fields"]


def test_redelivery_and_replay_do_not_create_new_versions(plane):
    sample = CORPUS / "official-candidate-20261014"
    _deliver(plane, [sample])
    _deliver(plane, [sample])                    # 同一事件再次投递，同一文档再次投放
    dom_db = plane[1]
    assert [f["version"] for f in _facts(dom_db)] == [1]
    assert len(_events(dom_db, "domain.fact.upserted.v1")) == 1


def test_new_snapshot_with_changed_values_is_a_new_version_with_stable_result_ids(plane):
    evd_db, dom_db, pipeline, materializer, _ = plane
    _deliver(plane, [CORPUS / "official-award-20261014"])
    v1 = _facts(dom_db)[0]
    reader = materializer.extractions
    first = _events(evd_db, "evidence.extraction.revised.v1")[0]["data"]["revision_ref"]
    revision = reader.revision(first)
    changed = copy.deepcopy(list(revision.facts))
    changed[0]["lots"][0]["bid_results"][0]["price"]["value"] = "695891"
    for r in changed[0]["lots"][1]["bid_results"]:
        r["id"] = "bdr_" + "Z" * 26              # 新快照的解析器分配了新标识
    later = dataclasses.replace(revision, id="exr_" + "1" * 26, facts=tuple(changed))
    with reqctx.use(reqctx.service("domain-intelligence")), dom_db.tx() as conn:
        (v2,) = materializer.materialize(conn, later)
    assert v2["id"] == v1["id"] and v2["version"] == 2
    assert v2["lots"][1]["bid_results"][0]["id"] == v1["lots"][1]["bid_results"][0]["id"]
    event = _events(dom_db, "domain.fact.upserted.v1")[-1]["data"]
    assert event["fact_version"] == 2 and event["changed_fields"] == ["/lots"]
    keys = lambda f: [u["key"] for u in bid_result_units(f)]  # noqa: E731
    assert keys(v1) == keys(v2)                  # 信号去重键跨版本不变
    # 取值相同、只有溯源不同的修订不产生新版本
    same = dataclasses.replace(revision, id="exr_" + "2" * 26, facts=tuple(changed))
    with reqctx.use(reqctx.service("domain-intelligence")), dom_db.tx() as conn:
        assert materializer.materialize(conn, same) == []


def test_suspect_and_inconsistent_events_are_not_materialized(plane):
    evd_db, dom_db, pipeline, materializer, _ = plane
    replay(evd_db, pipeline, [CORPUS / "official-award-20261014"])
    env = _events(evd_db, "evidence.extraction.revised.v1")[0]
    suspect = copy.deepcopy(env)
    suspect["data"]["quality_status"] = "suspect"
    with reqctx.use(reqctx.service("domain-intelligence")), dom_db.tx() as conn:
        materializer.on_extraction_revised(conn, suspect)
    assert _facts(dom_db) == []
    widened = copy.deepcopy(env)
    widened["data"]["rights_tags"] = ["public-official-review"]          # 事件丢掉了样本的权利标签
    forged = copy.deepcopy(env)
    forged["source"] = "miaoqia/acquisition"
    tenant = copy.deepcopy(env)
    tenant.update(miaoqiazone="tenant", miaoqiatenant="tnt_01J0000000000000000000000A", miaoqiacell="cell-01")
    missing = copy.deepcopy(env)
    missing["data"]["revision_ref"] = "exr_" + "9" * 26
    for bad, error in ((widened, ValueError), (forged, ValueError), (tenant, ValueError), (missing, LookupError)):
        with pytest.raises(error), reqctx.use(reqctx.service("domain-intelligence")), dom_db.tx() as conn:
            materializer.on_extraction_revised(conn, bad)
    assert _facts(dom_db) == [] and _events(dom_db, "domain.fact.upserted.v1") == []


def test_contract_violation_rolls_back(plane):
    evd_db, dom_db, pipeline, materializer, _ = plane
    replay(evd_db, pipeline, [CORPUS / "official-award-20261014"])
    ref = _events(evd_db, "evidence.extraction.revised.v1")[0]["data"]["revision_ref"]
    revision = materializer.extractions.revision(ref)
    bad = copy.deepcopy(list(revision.facts))
    bad[0]["lots"][0]["bid_results"][0]["stage"] = "winner"            # 契约枚举外的取值
    with pytest.raises(Exception), reqctx.use(reqctx.service("domain-intelligence")), dom_db.tx() as conn:
        materializer.materialize(conn, dataclasses.replace(revision, facts=tuple(bad)))
    assert _facts(dom_db) == [] and _events(dom_db, "domain.fact.upserted.v1") == []


def test_versions_are_immutable_and_current_version_only_moves_forward(plane):
    _deliver(plane, [CORPUS / "official-award-20261014"])
    dsn = plane[4]["app"]
    for statement in ("UPDATE dom_fact_versions SET content = '{}'::jsonb", "DELETE FROM dom_fact_versions",
                      "UPDATE dom_facts SET current_version = 1", "DELETE FROM dom_facts",
                      "UPDATE dom_facts SET current_version = 2, source_key = 'x'"):
        with psycopg.connect(dsn) as c, pytest.raises(psycopg.errors.IntegrityConstraintViolation):
            c.execute(statement)


def test_bid_result_view_follows_the_a02_rules(plane):
    _deliver(plane, [CORPUS / f"official-{n}-20261014" for n in
                     ("award", "candidate", "multiwinner", "ordinary-multiwinner", "award-amended-original", "consortium")])
    facts = {f["rights_tags"][1].removeprefix("corpus:official-").removesuffix("-20261014"): f for f in _facts(plane[1])}
    award = bid_result_units(facts["award"])
    assert len(award) == 4 and {u["fields"]["result_type"] for u in award} == {"awarded"}
    assert award[0]["fields"]["amount"] == {"value": "695890", "currency": "CNY"} and award[0]["display"]["amount_cny"] == 695890
    assert award[0]["subject"] == {"id": facts["award"]["id"], "version": 1, "part_id": facts["award"]["lots"][0]["bid_results"][0]["id"]}
    candidates = bid_result_units(facts["candidate"])
    assert [u["fields"]["result_type"] for u in candidates] == ["candidate_published"] * 3
    assert not any("amount" in u["fields"] for u in candidates)         # 投标报价未标明总价含义，不进入金额
    assert bid_result_units(facts["multiwinner"]) == []                  # 框架入围不是第二阶段成交
    unit_sum = bid_result_units(facts["ordinary-multiwinner"])
    assert len(unit_sum) == 5 and not any("amount" in u["fields"] for u in unit_sum)   # 年单价合计不当总额
    assert bid_result_units(facts["award-amended-original"]) == []       # 中标人只在附件中，不伪造
    (consortium,) = bid_result_units(facts["consortium"])
    assert consortium["fields"]["winner"]["uscc"] == "91321283MADWT8A11R"
    # 未经组织解析、行政区划解析与项目聚合的字段不出现（读取为未知），不以名称或标题代替
    for unit in award + candidates + unit_sum + [consortium]:
        assert not {"location", "project_id", "categories", "org_ids"} & set(unit["fields"])
        assert "org_id" not in unit["fields"]["winner"]


def test_materializer_runs_only_in_the_consumer_role_and_requires_its_binding():
    from miaoqia.domain_intelligence import module
    from miaoqia.kernel import runtime

    def env(role):
        return runtime.Env(role=role, brokers="unused", db=None, subsystem="domain-intelligence")

    assert module().consumers(env("data-api")) == []
    with pytest.raises(SystemExit):
        module().consumers(env("data-consumer"))      # 未装配解析修订读取时拒绝启动，不以空数据运行


def test_source_reverting_to_earlier_content_restores_the_current_value(plane):
    """A→A 无新版本；A→B→A 产生新快照与新版本，当前值回到 A，并发布事实事件（评审 P1 回归）。"""
    from miaoqia.evidence.replay import corpus_artifact
    evd_db, dom_db, pipeline, materializer, _ = plane
    a = corpus_artifact(CORPUS / "official-consortium-20261014")
    b = dataclasses.replace(a, raw=a.raw.replace("4186666元".encode(), "4186667元".encode()))
    seen: set[str] = set()

    def ingest(art):
        with reqctx.use(reqctx.service("evidence")), evd_db.tx() as conn:
            result = pipeline.ingest(conn, art)
        for env in _events(evd_db, "evidence.extraction.revised.v1"):
            if env["id"] not in seen:
                seen.add(env["id"])
                with reqctx.use(reqctx.service("domain-intelligence")), dom_db.tx() as conn:
                    materializer.on_extraction_revised(conn, env)
        (fact,) = _facts(dom_db)
        return result.snapshot_created, fact["version"], fact["lots"][0]["bid_results"][0]["price"]["value"]

    assert ingest(a) == (True, 1, "4186666")
    assert ingest(a) == (False, 1, "4186666")
    assert ingest(b) == (True, 2, "4186667")
    assert ingest(a) == (True, 3, "4186666")
    assert [e["data"]["fact_version"] for e in _events(dom_db, "domain.fact.upserted.v1")] == [1, 2, 3]


def test_fact_marked_suspect_by_the_extractor_is_not_materialized(plane):
    """证据与契约都合法、但解析器主动标为 suspect 的事实：修订为 suspect，不物化（评审 P2 回归）；warning 照常物化。"""
    from miaoqia.sdk import ExtractionResult
    evd_db, dom_db, pipeline, materializer, _ = plane
    real = pipeline.extractors[[p.ref for p in pipeline.extractors].index("proc.result-notice@0.2.0")]

    def marking(status):
        class Marking:
            id, version, fact_types = "proc.marking-" + status, "0.0.1", ("tendering.notice",)

            def applies(self, doc, source):
                return True

            def extract(self, doc, ctx):
                fact = real.extractor.extract(doc, ctx).facts[0]
                fact["quality"] = {"status": status, "warnings": [{"code": "REVIEW_REQUIRED"}]}
                return ExtractionResult(facts=[fact])
        return Marking()

    pipeline.extractors = [dataclasses.replace(real, extractor=marking("suspect"))]
    _deliver(plane, [CORPUS / "official-award-20261014"])
    assert _events(evd_db, "evidence.extraction.revised.v1")[-1]["data"]["quality_status"] == "suspect"
    assert _facts(dom_db) == []
    pipeline.extractors = [dataclasses.replace(real, extractor=marking("warning"))]
    _deliver(plane, [CORPUS / "official-award-20261014"])
    assert _events(evd_db, "evidence.extraction.revised.v1")[-1]["data"]["quality_status"] == "warning"
    assert [f["quality"]["status"] for f in _facts(dom_db)] == ["warning"]


def test_read_binding_selects_recorded_versions_and_pins_without_future_leakage(plane):
    from datetime import datetime, timezone
    from miaoqia import domain_intelligence
    from miaoqia.domain_intelligence.api import Basis

    _deliver(plane, [CORPUS / "official-award-20261014"])
    v1 = _facts(plane[1])[0]
    cutoff = datetime.now(timezone.utc)
    ref = _events(plane[0], "evidence.extraction.revised.v1")[0]["data"]["revision_ref"]
    revision = plane[3].extractions.revision(ref)
    changed = copy.deepcopy(list(revision.facts))
    changed[0]["lots"][0]["bid_results"][0]["price"]["value"] = "695891"
    with reqctx.use(reqctx.service("domain-intelligence")), plane[1].tx() as conn:
        plane[3].materialize(conn, dataclasses.replace(revision, id="exr_" + "1" * 26, facts=tuple(changed)))
    reader = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: purpose == "object_page")
    ctx = dataclasses.replace(reqctx.service("test"), purpose="object_page")
    basis = Basis("known_at", cutoff)
    with reqctx.use(ctx):
        old = reader.get_fact(v1["id"], basis)
        assert old.basis == basis and old.fact == v1
        assert reader.get_fact(v1["id"], Basis("current")).fact["version"] == 2
        assert reader.get_fact(v1["id"], basis, version=2) is None
        assert reader.get_fact(v1["id"], Basis("current"), version=1).fact == v1
        page = reader.query_facts("INF-PROC", "tendering.notice", basis)
        assert page.basis == basis and page.items == (v1,)
        assert [r["fact"]["version"] for r in reader.get_fact_history(v1["id"], basis).items] == [1]
        first = reader.get_fact_history(v1["id"], Basis("current"), limit=1)
        assert first.next_cursor == "1"
        assert [r["fact"]["version"] for r in reader.get_fact_history(v1["id"], Basis("current"), after=int(first.next_cursor)).items] == [2]
        assert reader.bid_result_units(v1["id"], basis).items[0]["subject"]["version"] == 1
    for invalid in (lambda: Basis("business_at", cutoff), lambda: Basis("known_at"),
                    lambda: Basis("known_at", datetime(2026, 1, 1)), lambda: Basis("current", cutoff)):
        with pytest.raises(ValueError):
            invalid()


def test_read_binding_checks_caller_and_current_rights_for_every_shape(plane):
    from miaoqia import domain_intelligence
    from miaoqia.domain_intelligence.api import Basis

    _deliver(plane, [CORPUS / "official-award-20261014", CORPUS / "official-consortium-20261014"])
    fact = _facts(plane[1])[0]
    permitted = "tnt_01J0000000000000000000000A"
    denied = "tnt_01J0000000000000000000B"
    seen = []

    def policy(tags, purpose):
        ctx = reqctx.current()
        seen.append((tags, ctx.tenant, purpose))
        return ctx.tenant == permitted and purpose == "object_page"

    reader = domain_intelligence.queries(plane[1], allowed=policy)
    basis = Basis("current")
    for tenant, yes in ((permitted, True), (denied, False)):
        ctx = reqctx.Context(reqctx.Principal("usr_test", "user"), tenant=tenant, cell="cell-01", purpose="object_page")
        with reqctx.use(ctx):
            assert (reader.get_fact(fact["id"], basis) is not None) == yes
            assert bool(reader.get_fact_history(fact["id"], basis).items) == yes
            assert bool(reader.query_facts("INF-PROC", "tendering.notice", basis).items) == yes
            assert bool(reader.bid_result_units(fact["id"], basis).items) == yes
            assert reader.query_facts("INF-PROC' OR true --", "tendering.notice", basis).items == ()
            for limit in (0, -1, 101, True):
                with pytest.raises(ValueError):
                    reader.query_facts("INF-PROC", "tendering.notice", basis, limit=limit)
    assert seen and all("public-official-review" in tags for tags, _, _ in seen)
    with reqctx.use(dataclasses.replace(reqctx.service("test"), purpose="object_page")), pytest.raises(PermissionError):
        domain_intelligence.queries(plane[1]).get_fact(fact["id"], basis)
    with reqctx.use(reqctx.service("test")), pytest.raises(PermissionError):
        reader.get_fact(fact["id"], basis)
    with pytest.raises(reqctx.MissingContext):
        reader.get_fact(fact["id"], basis)


@pytest.mark.parametrize("populated", [False, True])
@pytest.mark.parametrize("missing", ["policy", "purpose", "context"])
def test_read_preconditions_do_not_depend_on_matching_rows(plane, populated, missing):
    from contextlib import nullcontext
    from miaoqia import domain_intelligence
    from miaoqia.domain_intelligence.api import Basis

    if populated:
        _deliver(plane, [CORPUS / "official-award-20261014"])
    fact_id = _facts(plane[1])[0]["id"] if populated else "ntc_" + "Z" * 26
    reader = domain_intelligence.queries(plane[1], allowed=None if missing == "policy" else lambda tags, purpose: True)
    ctx = dataclasses.replace(reqctx.service("test"), purpose="" if missing == "purpose" else "object_page")
    basis = Basis("current")
    calls = (
        lambda: reader.get_fact(fact_id, basis),
        lambda: reader.get_fact_history(fact_id, basis),
        lambda: reader.query_facts("INF-PROC", "tendering.notice", basis),
        lambda: reader.bid_result_units(fact_id, basis),
    )
    with nullcontext() if missing == "context" else reqctx.use(ctx):
        for call in calls:
            with pytest.raises(reqctx.MissingContext if missing == "context" else PermissionError):
                call()


def test_fact_query_pagination_does_not_duplicate_or_omit_permitted_facts(plane):
    from miaoqia import domain_intelligence
    from miaoqia.domain_intelligence.api import Basis
    _deliver(plane, [CORPUS / f"official-{n}-20261014" for n in ("award", "candidate", "consortium")])
    reader = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
    found, after = [], ""
    with reqctx.use(dataclasses.replace(reqctx.service("test"), purpose="object_page")):
        while True:
            page = reader.query_facts("INF-PROC", "tendering.notice", Basis("current"), after=after, limit=1)
            found.extend(f["id"] for f in page.items)
            if page.next_cursor is None:
                break
            after = page.next_cursor
    assert found == sorted(f["id"] for f in _facts(plane[1]))


def test_building_pair_replays_to_readable_facts_and_no_invented_total(plane):
    from miaoqia import domain_intelligence
    from miaoqia.domain_intelligence.api import Basis
    _deliver(plane, [CORPUS / f"official-building-{kind}-20261026" for kind in ("tender", "award")])
    reader = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
    with reqctx.use(dataclasses.replace(reqctx.service("test"), purpose="object_page")):
        facts = reader.query_facts("INF-PROC", "tendering.notice", Basis("current")).items
        assert len(facts) == 2
        assert {f["project_code"]["value"] for f in facts} == {"XCXCG-2025-008"}
        award = next(f for f in facts if f["notice_type"] == "award")
        (unit,) = reader.bid_result_units(award["id"], Basis("current")).items
        assert unit["fields"]["winner"]["name_raw"] == "新兴铸管股份有限公司"
        assert "amount" not in unit["fields"] and "amount_cny" not in unit["display"]
        denied = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: False)
        page = denied.query_facts("INF-PROC", "tendering.notice", Basis("current"), limit=1)
        assert page.items == () and page.next_cursor is None
    assert len(_events(plane[1], "domain.fact.upserted.v1")) == 4
