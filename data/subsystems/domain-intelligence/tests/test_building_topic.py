"""有界主题：独立YAML矩阵、实际CLI和固定历史/项目/权利负例。"""
import copy
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest
from sqlalchemy import text
from miaoqia import domain_intelligence, evidence
from miaoqia.domain_intelligence.api import Basis
from miaoqia.domain_intelligence.domain.views import bid_result_units
from miaoqia.kernel import reqctx
from miaoqia.sdk.derivation import fingerprint
from test_facts import REPO, _facts, _events, plane  # noqa: F401
from test_projects import _projects
from test_enrichments import CTX, _setup, _service, _register, _apply, _counts


def _recipe():
    return evidence.load_recipe(REPO / "packs/industries/building-materials",
        "miaoqia_pack_building_materials.topic:build_recipe", REPO / "packs")


def _topic(plane):
    recipe = _recipe()
    service = _service(plane, enabled=False, recipes=[recipe])
    with reqctx.use(CTX):
        service.register_reference(**recipe.reference, content=recipe.content, rights_tags=["review:building-topic-pair"])
    return service, recipe


def test_independent_matrix_and_all_yaml_terms_exclusions():
    recipe = _recipe()
    def codes(t): return {c["code"] for c in recipe.transform([t], [recipe.content])}
    cases = json.loads((REPO / "packs/industries/building-materials/topic-evaluation.json").read_text())
    for row in cases: assert codes(row["text"]) == set(row["codes"]), row
    products = recipe.content["vocabulary"]["vocabulary"]["products"]
    assert len(products) == 31 and sum(1 + len(p.get("synonyms", [])) for p in products) == 153
    for p in products:
        for term in [p["term"], *p.get("synonyms", [])]:
            assert codes(term) == {p["category"]}, term
            assert not codes("不采购" + term)
            assert not codes(term + "或其他产品")
            for veto in p.get("not_when", []): assert p["category"] not in codes(veto + term)
    from miaoqia_pack_building_materials.topic import EXPECTED_REFERENCE
    assert fingerprint(recipe.content) == EXPECTED_REFERENCE


def test_actual_pair_cli_fixed_evidence_history_rights_and_retry(plane):
    _setup(plane)
    notices, before_project = _facts(plane[1]), _projects(plane)[0]
    cutoff = datetime.now(timezone.utc)
    env = dict(os.environ, MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN=plane[4]["app"],
        MIAOQIA_EVIDENCE_DB_DSN=plane[0].engine.url.render_as_string(hide_password=False).replace("postgresql+psycopg:", "postgresql:"),
        MIAOQIA_EVIDENCE_OBJECT_STORE_DIR=str(plane[2].store.root))
    args = [str(Path(sys.executable).parent / "mq-notice-enrich"), "--recipe", "proc.building-topic@0.1.0",
            "--packs", str(REPO / "packs"), *[n["id"] for n in notices]]
    denied = subprocess.run(args, env=env, capture_output=True, text=True)
    assert denied.returncode == 2 and "未启用" in denied.stderr
    args.insert(1, "--enable-building-topic")
    first = subprocess.run(args, env=env, capture_output=True, text=True)
    assert first.returncode == 0, first.stderr
    assert {r["action"] for r in json.loads(first.stdout)} == {"applied"}
    after = _counts(plane)
    retry = subprocess.run(args, env=env, capture_output=True, text=True)
    assert retry.returncode == 0, retry.stderr
    assert {r["action"] for r in json.loads(retry.stdout)} == {"unchanged"} and after == _counts(plane)
    service, recipe = _topic(plane)
    with reqctx.use(CTX):
        query = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
        for full in _facts(plane[1]):
            assert full["schema_version"] == "1.2.0" and full["version"] == 2
            assert [x["category"]["code"] for x in full["subject_categories"]] == ["CAT-PIPE-DI"]
            assert full["subject_categories"][0]["category"]["confidence"] == 0.95
            assert full["subject_categories"][0]["prov"]["confidence"] == 0.95
            assert "subject_categories" not in query.get_fact(full["id"], Basis("current"), version=1).fact
            assert "subject_categories" not in query.get_fact(full["id"], Basis("known_at", cutoff)).fact
            target = next(n for n in notices if n["id"] == full["id"])
            key = service.register(target, recipe.id)
            result = service.get(key)
            guard = result["outputs"][0]["scope_check"]
            assert guard["eligible"] is True and guard["title_spans"] == [[6, 11]]
            assert result["outputs"][0]["diagnostics"]["rule_score"] == 1
            assert service.resolve(result["outputs"][0]["evidence_ref"]).derivation["inputs"][0]["field_path"] == "/title/value"
            assert query.bid_result_units(full["id"], Basis("known_at", cutoff)).items == tuple(bid_result_units(target))
        award = next(n for n in _facts(plane[1]) if n["notice_type"] == "award")
        unit, = query.bid_result_units(award["id"], Basis("current")).items
        assert unit["fields"]["categories"] == ["CAT-PIPE-DI"] and unit["fields"]["categories_scope"] == "notice_subject"
        assert unit["evidence"]["fact.categories"]["confidence"] == 0.95
        assert "amount" not in unit["fields"] and "awarded_items" not in unit["fields"]
        with plane[1].tx() as conn:
            link = conn.execute(text("SELECT * FROM dom_notice_links WHERE notice_id=:n AND notice_version=2"), {"n": award["id"]}).mappings().one()
        assert unit["item_refs"][-1]["version"] == link["reason"]["project_version"]
        assert unit["item_refs"][-1]["id"] == before_project["id"]
        denied = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: "review:building-topic-pair" not in tags)
        assert denied.bid_result_units(award["id"], Basis("current")).items == ()
        restricted = _service(plane, enabled=False, recipes=[recipe], display=lambda tags, purpose: "none" if "review:building-topic-pair" in tags else "full_text")
        assert restricted.get(key) is None
        with pytest.raises(ValueError, match="不得改"):
            service.register_reference(**recipe.reference, content={"eligible": True}, rights_tags=["review:building-topic-pair"])
    assert len(_events(plane[1], "tendering.result.published.v1")) == 1
    assert len(_events(plane[1], "tendering.project.linked.v1")) == 2


@pytest.mark.parametrize("change", ["candidate", "framework", "second_result", "second_lot", "ending", "pending"])
def test_pure_view_never_broadcasts_topic_across_ambiguity(plane, change):
    _setup(plane)
    award = next(n for n in _facts(plane[1]) if n["notice_type"] == "award")
    service, recipe = _topic(plane)
    key, parent = _register(plane, service, award, recipe.id)
    _apply(plane, service, key, parent)
    award = next(n for n in _facts(plane[1]) if n["notice_type"] == "award")
    context = {"project": _projects(plane)[0], "categories": award["subject_categories"]}
    bad = copy.deepcopy(award); result = bad["lots"][0]["bid_results"][0]
    if change == "candidate": result["stage"] = "candidate"
    if change == "framework": result["result_kind"] = "framework_shortlist"
    if change == "second_result": bad["lots"][0]["bid_results"].append({**result, "is_winner": False})
    if change == "second_lot": bad["lots"].append({"lot_key": "2"})
    if change == "ending": bad["lots"][0]["ending"] = {"ending_type": "failed_bid"}
    if change == "pending": bad["result_details_pending"] = True
    assert all("categories" not in u["fields"] for u in bid_result_units(bad, context))
    assert all("categories" not in u["fields"] for u in bid_result_units(award))


def test_low_confidence_clears_stale_owned_field_and_unknown_raw_guard(plane):
    """真实单事实修订夹具：低证据源已有旧topic，必须删除而非累加。"""
    import dataclasses
    from miaoqia.kernel import ids
    from test_projects import _revisions, _materialize
    revision = _revisions(plane)[1]
    fact = copy.deepcopy(revision.facts[0])
    fact["title"]["prov"]["confidence"] = 0.79
    fact["subject_categories"] = [{"category": {"scheme": "industry:building-materials", "scheme_version": "0.2.0", "code": "CAT-FIT-DI", "confidence": 0.79},
        "prov": fact["title"]["prov"], "scope": "notice_subject", "source_field": "/title/value"}]
    revision_id = ids.new("exr")
    with reqctx.use(CTX), plane[0].tx() as conn:
        conn.execute(text("""INSERT INTO evd_extraction_revisions(id,snapshot_id,extractor,pack_id,pack_version,facts,quality_status,warnings)
            VALUES (:r,:s,'manual-lowconfidence-fixture@0.1.0','INF-PROC','1.6.1',CAST(:f AS jsonb),'ok','[]')"""),
            {"r": revision_id, "s": revision.snapshot_ref, "f": json.dumps([fact])})
    _materialize(plane, plane[3].extractions.revision(revision_id))
    service, recipe = _topic(plane)
    notice, = _facts(plane[1])
    key, parent = _register(plane, service, notice, recipe.id)
    with reqctx.use(CTX):
        output, = service.get(key)["outputs"]
        assert output["value"] == [] and output["diagnostics"]["candidates"]
        assert output["scope_check"]["eligible"] is True and output["diagnostics"]["rule_score"] == 0
        threshold_source = copy.deepcopy(notice)
        threshold_source["title"]["prov"]["confidence"] = 0.8
        reviewed = next(r for r in recipe.content["scope"]["reviews"] if r["title"] == notice["title"]["value"])
        threshold = recipe.outputs(threshold_source, output["diagnostics"]["candidates"], {"id": output["evidence_ref"]}, reviewed)[0]
        assert threshold["diagnostics"]["rule_score"] == 1 and threshold["value"][0]["category"]["confidence"] == 0.8
        assert _apply(plane, service, key, parent)["action"] == "applied"
        full, = _facts(plane[1])
        assert "subject_categories" not in full and full["version"] == 2
        source = copy.deepcopy(notice)
        source["title"]["prov"]["confidence"] = 1
        for wrong in ({"source_id": "wrong"}, {"source_key": "wrong"}, {"raw_sha256": "sha256:" + "0"*64}):
            original = recipe.content["scope"]["reviews"][0]
            snapshot = {k: original[k] for k in ("source_id", "source_key", "raw_sha256")}
            snapshot.update(wrong)
            result = recipe.outputs(source, [{"code": "CAT-PIPE-DI", "span": [6, 11], "rule_score": 1}], {"id": output["evidence_ref"]}, snapshot)[0]
            assert result["value"] == [] and result["scope_check"]["eligible"] is False


def test_fixed_pp_dependency_rights_and_invalid_receipt_cannot_authorize(plane):
    _setup(plane)
    service, recipe = _topic(plane)
    award = next(n for n in _facts(plane[1]) if n["notice_type"] == "award")
    key, parent = _register(plane, service, award, recipe.id)
    _apply(plane, service, key, parent)
    with reqctx.use(CTX):
        query = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
        award = query.get_fact(award["id"], Basis("current")).fact
        assert query.bid_result_units(award["id"], Basis("current")).items[0]["fields"]["categories"] == ["CAT-PIPE-DI"]
        # A later accepted enrichment advances current pp, not this fixed award link.
        fixed_project_ref = query.bid_result_units(award["id"], Basis("current")).items[0]["item_refs"][-1]
        tender = next(n for n in _facts(plane[1]) if n["notice_type"] == "tender")
        identity = _service(plane)
        identity_key, parent = _register(plane, identity, tender, "test.buyer-provenance@0.1.0")
        _apply(plane, identity, identity_key, parent)
        assert _projects(plane)[0]["version"] > fixed_project_ref["version"]
        assert query.bid_result_units(award["id"], Basis("current")).items[0]["item_refs"][-1] == fixed_project_ref
        # Other member right denied: selected award is readable, pp/dependencies are not.
        denied = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: "corpus:official-building-tender-20261026" not in tags)
        assert denied.get_fact(award["id"], Basis("current")) is not None
        assert "categories" not in denied.bid_result_units(award["id"], Basis("current")).items[0]["fields"]
        # Test query's immutable-proof verification using real rows but a wrong pp version/context.
        with plane[1].tx() as conn:
            assert query._topic_context(conn, {**award, "version": 1}, Basis("current")) is None
            proof = conn.execute(text("SELECT * FROM dom_enrichment_receipts WHERE run_key=:k"), {"k": key}).mappings().one()
            assert proof["outputs"][0]["scope_check"]["source_hash"] == proof["source_hash"]
        with pytest.raises(ValueError, match="未启用"):
            _service(plane, enabled=False).get(key)


@pytest.mark.parametrize("mutation", ["eligible", "span", "category", "path", "reference"])
def test_recipe_rebuild_rejects_forged_metadata_even_beyond_object_hash(plane, monkeypatch, mutation):
    _setup(plane)
    service, recipe = _topic(plane)
    key, _ = _register(plane, service, _facts(plane[1])[0], recipe.id)
    original = service._load
    def forged(row):
        value = copy.deepcopy(original(row))
        if "run_key" in value:
            out = value["outputs"][0]
            if mutation == "eligible": out["scope_check"]["eligible"] = False
            if mutation == "span": out["scope_check"]["title_spans"] = [[0, 1]]
            if mutation == "category": out["value"][0]["category"]["code"] = "CAT-FIT-DI"
            if mutation == "path": out["path"] = "/buyer"
            if mutation == "reference": value["reference_hash"] = "sha256:" + "0" * 64
        return value
    monkeypatch.setattr(service, "_load", forged)
    with reqctx.use(CTX), pytest.raises(ValueError): service.get(key)


def test_new_raw_revision_same_title_remains_unknown_and_old_task_stale(plane):
    import dataclasses
    from miaoqia.evidence.replay import corpus_artifact
    from miaoqia.domain_intelligence.application.enrichments import StaleEnrichment
    from test_projects import _materialize
    from test_facts import CORPUS
    _setup(plane)
    service, recipe = _topic(plane)
    award = next(n for n in _facts(plane[1]) if n["notice_type"] == "award")
    key, parent = _register(plane, service, award, recipe.id)
    artifact = corpus_artifact(CORPUS / "official-building-award-20261026")
    changed = dataclasses.replace(artifact, raw=artifact.raw.replace(b"76.5000", b"77.5000"))
    assert changed.raw != artifact.raw
    with reqctx.use(reqctx.service("evidence")), plane[0].tx() as conn: plane[2].ingest(conn, changed)
    ref = _events(plane[0], "evidence.extraction.revised.v1")[-1]["data"]["revision_ref"]
    _materialize(plane, plane[3].extractions.revision(ref))
    with pytest.raises(StaleEnrichment): _apply(plane, service, key, parent)
    revised = next(n for n in _facts(plane[1]) if n["id"] == award["id"])
    assert revised["title"]["value"] == award["title"]["value"]
    key, parent = _register(plane, service, revised, recipe.id)
    with reqctx.use(CTX):
        output, = service.get(key)["outputs"]
        assert output["diagnostics"]["candidates"] and output["diagnostics"]["rule_score"] == 0
        assert output["scope_check"]["eligible"] is False and output["value"] == []
    assert _apply(plane, service, key, parent)["action"] == "applied"  # 新增参考rights也构成真实版本变化
    with reqctx.use(CTX):
        query = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
        assert "categories" not in query.bid_result_units(revised["id"], Basis("current")).items[0]["fields"]
