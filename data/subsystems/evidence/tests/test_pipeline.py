"""公共文档管线：真实 PostgreSQL 上的快照、解析修订、证据引用与发件箱；含越权与反例。"""
from __future__ import annotations

import dataclasses
import json
import shutil
from datetime import datetime, timezone
from pathlib import Path

import psycopg
import pytest
from sqlalchemy import text

from miaoqia.evidence import MIGRATIONS
from miaoqia.evidence.application.pipeline import Pipeline, TenantScopeRejected
from miaoqia.evidence.extensions import packs
from miaoqia.evidence.infrastructure.objectstore import FilesystemObjectStore, ObjectConflict
from miaoqia.evidence.infrastructure.store import SqlExtractions
from miaoqia.evidence.replay import corpus_artifact, replay
from miaoqia.kernel import db, payloads, reqctx
from miaoqia.sdk import ExtractionResult

REPO = Path(__file__).resolve().parents[4]
CORPUS = REPO / "packs/information_domains/procurement/corpus"


@pytest.fixture
def evd(pg_factory, tmp_path):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    pipeline = Pipeline(FilesystemObjectStore(tmp_path / "objects"), packs.load(REPO / "packs"))
    yield database, pipeline, dsns
    database.close()


def _outbox(database):
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        return [r[0] for r in conn.execute(text("SELECT envelope FROM kernel_outbox ORDER BY seq"))]


def _count(database, table):
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        return conn.execute(text(f"SELECT count(*) FROM {table}")).scalar_one()


def test_replay_creates_snapshot_revision_evidence_and_valid_events(evd):
    database, pipeline, dsns = evd
    (result,) = replay(database, pipeline, [CORPUS / "official-award-20261014"])
    assert result.snapshot_created and len(result.revisions) == 1
    events = _outbox(database)
    assert [e["type"] for e in events] == ["evidence.snapshot.created.v1", "evidence.extraction.revised.v1"]
    for e in events:
        assert e["miaoqiazone"] == "public" and "miaoqiatenant" not in e and "miaoqiacell" not in e
        assert not list(payloads.validator(e["type"], {}).iter_errors(e["data"]))
    assert events[1]["data"]["rights_tags"] == ["public-official-review", "corpus:official-award-20261014"]
    reader = SqlExtractions(dsns["app"])
    revision = reader.revision(result.revisions[0])
    reader.close()
    assert revision.pack_id == "INF-PROC" and revision.extractor == "proc.result-notice@0.2.0"
    assert revision.source_key.startswith("https://www.ccgp.gov.cn/")
    # 每个字段的证据引用都已登记，并指向本快照
    refs = set()

    def walk(node):
        if isinstance(node, dict):
            if "evidence_ref" in node:
                refs.add(node["evidence_ref"])
            for v in node.values():
                walk(v)
        elif isinstance(node, list):
            for v in node:
                walk(v)

    walk(list(revision.facts))
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        stored = {r[0]: r[1] for r in conn.execute(text("SELECT id, snapshot_id FROM evd_evidence_refs"))}
    assert refs and refs <= set(stored) and set(stored.values()) == {result.snapshot_ref}


def test_replaying_the_same_document_is_idempotent(evd):
    database, pipeline, _ = evd
    sample = CORPUS / "official-candidate-20261014"
    first, = replay(database, pipeline, [sample])
    again, = replay(database, pipeline, [sample])
    assert not again.snapshot_created and again.snapshot_ref == first.snapshot_ref
    assert again.revisions == () and again.existing_revisions == first.revisions
    assert len(_outbox(database)) == 2 and _count(database, "evd_snapshots") == 1


def test_changed_content_of_the_same_source_object_is_a_new_snapshot(evd):
    database, pipeline, _ = evd
    art = corpus_artifact(CORPUS / "official-consortium-20261014")
    changed = dataclasses.replace(art, raw=art.raw.replace("4186666元".encode(), "4186667元".encode()))
    with reqctx.use(reqctx.service("evidence")):
        with database.tx() as conn:
            a = pipeline.ingest(conn, art)
        with database.tx() as conn:
            b = pipeline.ingest(conn, changed)
    assert a.snapshot_ref != b.snapshot_ref and b.snapshot_created
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        assert [r[0] for r in conn.execute(text("SELECT seq FROM evd_snapshots ORDER BY seq"))] == [1, 2]


def test_tenant_context_cannot_write_public_evidence(evd):
    """越权：带租户与单元的上下文调用公共管线被拒绝，且不留下任何快照或事件。"""
    database, pipeline, _ = evd
    art = corpus_artifact(CORPUS / "official-award-20261014")
    tenant = reqctx.Context(principal=reqctx.Principal("usr_x", "user"), tenant="tnt_01J0000000000000000000000A", cell="cell-01")
    with pytest.raises(TenantScopeRejected), reqctx.use(tenant), database.tx() as conn:
        pipeline.ingest(conn, art)
    assert _count(database, "evd_snapshots") == 0 and _outbox(database) == []


def test_published_evidence_is_immutable_for_the_application_account(evd):
    database, pipeline, dsns = evd
    replay(database, pipeline, [CORPUS / "official-award-20261014"])
    for statement in ("UPDATE evd_snapshots SET source_key = 'x'", "DELETE FROM evd_evidence_refs",
                      "UPDATE evd_extraction_revisions SET facts = '[]'::jsonb", "DELETE FROM evd_snapshots"):
        with psycopg.connect(dsns["app"]) as c, pytest.raises(psycopg.errors.IntegrityConstraintViolation):
            c.execute(statement)


def test_tampered_corpus_sample_is_rejected(tmp_path):
    sample = tmp_path / "official-award-20261014"
    shutil.copytree(CORPUS / "official-award-20261014", sample)
    raw = sample / "raw.html"
    raw.write_text(raw.read_text().replace("软通计算机有限公司", "某某公司", 1))
    with pytest.raises(ValueError, match="原件哈希"):
        corpus_artifact(sample)


def test_object_store_never_overwrites_and_rejects_escaping_keys(tmp_path):
    store = FilesystemObjectStore(tmp_path)
    store.put("raw/abc", b"one")
    store.put("raw/abc", b"one")
    with pytest.raises(ObjectConflict):
        store.put("raw/abc", b"two")
    assert store.get("raw/abc") == b"one"
    for key in ("../escape", "raw/../../x", "/etc/passwd", "other/abc"):
        with pytest.raises(ValueError):
            store.put(key, b"x")


def test_failed_semantic_validation_is_recorded_as_suspect(evd):
    """领域语义校验不通过的解析结果记为 suspect（进入复核），下游不物化。"""
    database, pipeline, _ = evd
    real = pipeline.extractors[[p.ref for p in pipeline.extractors].index("proc.result-notice@0.2.0")]

    def reject(document, facts, evidence):
        return ["人为注入的语义错误"]

    pipeline.extractors = [dataclasses.replace(real, semantic_validator=reject)]
    (result,) = replay(database, pipeline, [CORPUS / "official-award-20261014"])
    events = _outbox(database)
    assert events[-1]["data"]["quality_status"] == "suspect"
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        warnings = conn.execute(text("SELECT warnings FROM evd_extraction_revisions WHERE id = :i"), {"i": result.revisions[0]}).scalar_one()
    assert any(w["code"] == "VALIDATION_FAILED" for w in warnings)


def test_extractor_fields_without_matching_quote_are_suspect(evd):
    """解析器伪造的证据（定位处原文与取值不一致）被证据完整性校验拦下。"""
    database, pipeline, _ = evd
    real = pipeline.extractors[[p.ref for p in pipeline.extractors].index("proc.result-notice@0.2.0")]

    class Forger:
        id, version, fact_types = "proc.forger", "0.0.1", ("tendering.notice",)

        def applies(self, doc, source):
            return True

        def extract(self, doc, ctx):
            fact = real.extractor.extract(doc, ctx).facts[0]
            fact["title"]["value"] = "伪造的标题"
            return ExtractionResult(facts=[fact])

    pipeline.extractors = [dataclasses.replace(real, extractor=Forger())]
    replay(database, pipeline, [CORPUS / "official-award-20261014"])
    assert _outbox(database)[-1]["data"]["quality_status"] == "suspect"


def test_pack_entrypoint_outside_the_pack_is_rejected(tmp_path):
    pack = tmp_path / "information_domains/evil"
    (pack / "src/evil_pack").mkdir(parents=True)
    (pack / "src/evil_pack/__init__.py").write_text("")
    (pack / "src/evil_pack/validation.py").write_text("def validate(document, facts, evidence):\n    return []\n")
    (pack / "pack.yaml").write_text(json.dumps({"pack": {"id": "INF-EVIL", "version": "1.0.0", "provides": {
        "semantic_validator": "evil_pack.validation:validate",
        "extractors": ["miaoqia.evidence.replay:Pipeline"]}}}))
    with pytest.raises(ValueError, match="不在包内"):
        packs.load(tmp_path)


def test_html_only_unless_reviewed_normalization_is_given(evd):
    database, pipeline, _ = evd
    art = corpus_artifact(CORPUS / "official-ordinary-multiwinner-20261014")
    assert art.media_type == "application/pdf" and art.normalized is not None
    with reqctx.use(reqctx.service("evidence")), database.tx() as conn:
        assert pipeline.ingest(conn, art).revisions
    bare = dataclasses.replace(art, normalized=None, source_key=art.source_key + "#bare")
    with pytest.raises(ValueError, match="不支持"), reqctx.use(reqctx.service("evidence")), database.tx() as conn:
        pipeline.ingest(conn, bare)
    assert datetime.now(timezone.utc) > art.observed_at


def test_replay_events_carry_a_contract_valid_root_traceparent_inherited_downstream(evd):
    """回归：下游消费者在回放事件的追踪上下文中写发件箱，traceparent 须符合信封契约（标志 0[01]）。"""
    import re

    from opentelemetry import trace

    from miaoqia.kernel import outbox, tracing
    database, pipeline, _ = evd
    replay(database, pipeline, [CORPUS / "official-award-20261014"])
    events = _outbox(database)
    assert all(re.fullmatch(r"00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]", e["traceparent"]) for e in events)
    tracing.setup("data", "test")
    try:
        with tracing.tracer("t").start_as_current_span("consume", context=tracing.context_from(events[-1]["traceparent"]),
                                                       kind=trace.SpanKind.CONSUMER):
            child = tracing.traceparent()
    finally:
        tracing.shutdown()
    assert child.split("-")[1] == events[-1]["traceparent"].split("-")[1]
    outbox.validate({**events[-1], "traceparent": child})


def test_resolve_evidence_clips_by_current_rights_before_reading_objects(evd):
    from miaoqia import evidence
    database, pipeline, _ = evd
    replay(database, pipeline, [CORPUS / "official-award-20261014"])
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        ref = conn.execute(text("SELECT id FROM evd_evidence_refs ORDER BY id LIMIT 1")).scalar_one()

    class NoRead:
        def get(self, key):
            raise AssertionError("被拒绝或仅引证时不能访问正文")

    ctx = dataclasses.replace(reqctx.service("test"), purpose="object_page")
    with reqctx.use(ctx):
        assert evidence.resolver(database, NoRead(), display=lambda tags, purpose: "none").resolve(ref) is None
        citation = evidence.resolver(database, NoRead(), display=lambda tags, purpose: "citation_only").resolve(ref)
        assert citation.source_key.startswith("https://") and citation.snippet is None and citation.context is None and citation.locator is None
        excerpt = evidence.resolver(database, pipeline.store, display=lambda tags, purpose: "excerpt").resolve(ref)
        full = evidence.resolver(database, pipeline.store, display=lambda tags, purpose: "full_text").resolve(ref)
        assert excerpt.snippet and excerpt.context is None
        assert full.snippet.startswith(excerpt.snippet) and full.snippet in full.context
        assert excerpt.locator == full.locator
        assert evidence.resolver(database, NoRead(), display=lambda tags, purpose: "full_text").resolve("ev_" + "Z" * 26) is None
        with pytest.raises(PermissionError):
            evidence.resolver(database, NoRead()).resolve(ref)
        with pytest.raises(PermissionError):
            evidence.resolver(database, NoRead(), display=lambda tags, purpose: "typo").resolve(ref)
    with reqctx.use(reqctx.service("test")), pytest.raises(PermissionError):
        evidence.resolver(database, NoRead(), display=lambda tags, purpose: "full_text").resolve(ref)


def test_resolve_rechecks_current_caller_and_does_not_cache_permission(evd):
    from miaoqia import evidence
    database, pipeline, _ = evd
    replay(database, pipeline, [CORPUS / "official-consortium-20261014"])
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        ref = conn.execute(text("SELECT id FROM evd_evidence_refs LIMIT 1")).scalar_one()
    allowed_tenant = "tnt_01J0000000000000000000000A"
    rights = {"active": True}

    def display(tags, purpose):
        assert "public-official-review" in tags and "corpus:official-consortium-20261014" in tags
        return "full_text" if rights["active"] and reqctx.current().tenant == allowed_tenant and purpose == "object_page" else "none"

    reader = evidence.resolver(database, pipeline.store, display=display)
    ctx = reqctx.Context(reqctx.Principal("usr_test", "user"), tenant=allowed_tenant, cell="cell-01", purpose="object_page")
    with reqctx.use(ctx):
        assert reader.resolve(ref).snippet
        rights["active"] = False
        assert reader.resolve(ref) is None
    rights["active"] = True
    with reqctx.use(dataclasses.replace(ctx, tenant="tnt_01J0000000000000000000000B")):
        assert reader.resolve(ref) is None


def test_resolve_reuses_sdk_integrity_checks_for_tampered_objects(evd):
    from miaoqia import evidence
    from miaoqia.evidence.application.queries import EvidenceIntegrityError
    database, pipeline, _ = evd
    replay(database, pipeline, [CORPUS / "official-consortium-20261014"])
    with reqctx.use(reqctx.service("test")), database.tx() as conn:
        ref = conn.execute(text("SELECT id FROM evd_evidence_refs LIMIT 1")).scalar_one()

    class Tampered:
        def get(self, key):
            doc = json.loads(pipeline.store.get(key))
            doc["snapshot_ref"] = "snp_" + "Z" * 26
            return json.dumps(doc).encode()

    with reqctx.use(dataclasses.replace(reqctx.service("test"), purpose="object_page")), pytest.raises(EvidenceIntegrityError):
        evidence.resolver(database, Tampered(), display=lambda tags, purpose: "excerpt").resolve(ref)
