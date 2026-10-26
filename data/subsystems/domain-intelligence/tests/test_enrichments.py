"""真实PG两阶段：复用事实/采购项目harness，测试recipe不提供生产解析能力。"""
import dataclasses
from concurrent.futures import ThreadPoolExecutor
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
from miaoqia.domain_intelligence.application import facts, projects
from miaoqia.domain_intelligence.application.enrichments import StaleEnrichment, apply, prepare
from miaoqia.evidence.application.enrichments import REFERENCE, TEST_CONTENT
from miaoqia.evidence.replay import corpus_artifact
from miaoqia.kernel import ids, reqctx

from test_facts import CORPUS, _events, _facts, plane  # noqa: F401
from test_projects import _materialize, _projects, _revisions

CTX = dataclasses.replace(reqctx.service("test"), purpose="object_page")
BUYER = "test.buyer-provenance@0.1.0"
WINNER = "test.winner-provenance@0.1.0"


def _service(plane, display=None, store=None, enabled=True, recipes=()):
    reader = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
    def fixed(i, v):
        found = reader.get_fact(i, Basis("current"), version=v)
        return found.fact if found else None
    return evidence.enrichments(plane[0], store or plane[2].store, facts=fixed,
        display=display or (lambda tags, purpose: "full_text"), enable_test_recipe=enabled, recipes=recipes)


def _setup(plane, recipes=()):
    revisions = _revisions(plane)
    assert all(len(r.facts) == 1 for r in revisions)
    for revision in revisions:
        _materialize(plane, revision)
    service = _service(plane, recipes=recipes)
    with reqctx.use(CTX):
        service.register_reference(**REFERENCE, content=TEST_CONTENT, rights_tags=["test:identity-policy"])
        for recipe in recipes:
            service.register_reference(**recipe.reference, content=recipe.content, rights_tags=["review:building-topic-pair"])
    return service


def _register(plane, service, notice, recipe):
    with reqctx.use(CTX):
        target = prepare(plane[1], plane[3].extractions, [notice["id"]])[0]
        key = service.register(target["source"], recipe)
    return key, target["expected_parent"]


def _apply(plane, service, key, parent):
    with reqctx.use(CTX):
        return apply(plane[1], plane[3].extractions, service, key, expected_parent=parent)


def _counts(plane):
    with reqctx.use(CTX), plane[1].tx() as conn:
        return {t: conn.execute(text(f"SELECT count(*) FROM {t}")).scalar_one() for t in
                ("dom_fact_versions", "dom_notice_links", "dom_project_dependencies", "dom_enrichment_receipts", "kernel_outbox")}


def _assert_deps(plane, project):
    with reqctx.use(CTX), plane[1].tx() as conn:
        refs = conn.execute(text("SELECT notice_id,notice_version FROM dom_project_dependencies WHERE project_id=:p AND project_version=:v"),
                            {"p": project["id"], "v": project["version"]}).all()
    assert sorted(refs) == sorted((r["id"], r["version"]) for r in project["notice_refs"])
    assert set(project["notice_ids"]) == {r["id"] for r in project["notice_refs"]}
    assert len(refs) == len(project["notice_ids"])


def test_real_console_register_apply_retry_disabled_and_no_config(plane):
    _setup(plane)
    notice = _facts(plane[1])[0]
    old_project = _projects(plane)[0]
    cutoff = datetime.now(timezone.utc)
    env = dict(os.environ, MIAOQIA_DOMAIN_INTELLIGENCE_DB_DSN=plane[4]["app"],
        MIAOQIA_EVIDENCE_DB_DSN=plane[0].engine.url.render_as_string(hide_password=False).replace("postgresql+psycopg:", "postgresql:"),
        MIAOQIA_EVIDENCE_OBJECT_STORE_DIR=str(plane[2].store.root))
    args = [str(Path(sys.executable).parent / "mq-notice-enrich"), "--recipe", BUYER, notice["id"]]
    denied = subprocess.run(args, env=env, capture_output=True, text=True)
    assert denied.returncode == 2 and "未启用" in denied.stderr
    args.insert(1, "--enable-test-recipe")
    first = subprocess.run(args, env=env, capture_output=True, text=True)
    assert first.returncode == 0, first.stderr
    assert json.loads(first.stdout)[0]["action"] == "applied"
    full = next(n for n in _facts(plane[1]) if n["id"] == notice["id"])
    assert full["version"] == 2 and full["buyer"]["name_raw"] == notice["buyer"]["name_raw"]
    assert full["buyer"]["prov"]["evidence_ref"] != notice["buyer"]["prov"]["evidence_ref"]
    assert full["observed_at"] == notice["observed_at"] and full["revision_ref"] == notice["revision_ref"]
    assert datetime.fromisoformat(full["recorded_at"].replace("Z", "+00:00")) > cutoff
    current = _projects(plane)[0]
    assert current["id"] == old_project["id"] and current["version"] == old_project["version"] + 1
    _assert_deps(plane, current)
    before = _counts(plane)
    retry = subprocess.run(args, env=env, capture_output=True, text=True)
    assert retry.returncode == 0, retry.stderr
    assert json.loads(retry.stdout)[0]["action"] == "unchanged" and _counts(plane) == before
    with reqctx.use(CTX):
        query = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
        assert query.get_fact(full["id"], Basis("known_at", cutoff)).fact == notice
        assert query.get_fact(full["id"], Basis("current"), version=1).fact == notice
        assert query.get_fact(current["id"], Basis("known_at", cutoff)).fact == old_project
        denied = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: "test:identity-policy" not in tags)
        assert denied.get_fact(full["id"], Basis("current")) is None
        assert denied.get_fact(current["id"], Basis("current")) is None
    missing = subprocess.run(args, env={k: v for k, v in env.items() if k != "MIAOQIA_EVIDENCE_OBJECT_STORE_DIR"}, capture_output=True, text=True)
    assert missing.returncode == 2 and "未配置" in missing.stderr
    assert len(_events(plane[1], "tendering.notice.published.v1")) == 2
    assert len(_events(plane[1], "tendering.project.linked.v1")) == 2
    assert len(_events(plane[1], "tendering.result.published.v1")) == 1


def test_winner_only_concurrent_registration_apply_and_recipe_only_receipt(plane):
    service = _setup(plane)
    notice = next(n for n in _facts(plane[1]) if n["notice_type"] == "award")
    project = _projects(plane)[0]
    def work(_):
        key, parent = _register(plane, service, notice, WINNER)
        return _apply(plane, service, key, parent)
    with ThreadPoolExecutor(max_workers=4) as pool:
        results = list(pool.map(work, range(4)))
    assert {r["version"] for r in results} == {2}
    after = _projects(plane)[0]
    assert after["version"] == project["version"] + 1
    assert {k: v for k, v in after.items() if k in ("buyer", "lots", "title", "notice_ids")} == {
        k: v for k, v in project.items() if k in ("buyer", "lots", "title", "notice_ids")}
    _assert_deps(plane, after)
    changed = _events(plane[1], "domain.fact.upserted.v1")[-1]["data"]["changed_fields"]
    assert changed == ["/notice_refs", "/rights_tags"]
    # buyer 0.1.1登记同一派生内容，recipe版本本身不会伪造新领域版本。
    key, parent = _register(plane, service, notice, BUYER)
    _apply(plane, service, key, parent)
    before = _counts(plane)
    key, parent = _register(plane, service, notice, "test.buyer-provenance@0.1.1")
    assert _apply(plane, service, key, parent)["action"] == "recorded"
    after_counts = _counts(plane)
    assert after_counts == {**before, "dom_enrichment_receipts": before["dom_enrichment_receipts"] + 1}


@pytest.mark.parametrize("recipe_id", [BUYER, "proc.building-topic@0.1.0"])
def test_payload_failure_domain_rolls_back_evidence_remains_and_retries(plane, monkeypatch, recipe_id):
    recipes = []
    if recipe_id != BUYER:
        recipes = [evidence.load_recipe(CORPUS.parents[2] / "industries/building-materials",
            "miaoqia_pack_building_materials.topic:build_recipe", CORPUS.parents[2])]
    service = _setup(plane, recipes)
    notice = _facts(plane[1])[0]
    key, parent = _register(plane, service, notice, recipe_id)
    before, original = _counts(plane), facts.payloads.validator
    def invalid_gate(name, *args, **kwargs):
        actual = original(name, *args, **kwargs)
        class Gate:
            def validate(self, data):
                if name == "domain.fact.upserted.v1" and data["object_type"] == projects.PROJECT_TYPE:
                    data["changed_fields"] = []
                return actual.validate(data)
        return Gate()
    monkeypatch.setattr(facts.payloads, "validator", invalid_gate)
    with pytest.raises(Exception, match="should be non-empty"):
        _apply(plane, service, key, parent)
    assert _counts(plane) == before
    with reqctx.use(CTX):
        assert service.get(key) is not None
    monkeypatch.setattr(facts.payloads, "validator", original)
    assert _apply(plane, service, key, parent)["action"] == "applied"
    assert len(_events(plane[0], "evidence.enrichment.completed.v1")) == 1


def test_stale_cas_and_new_source_revision_enhancement_remains_pending(plane):
    service = _setup(plane)
    notice = next(n for n in _facts(plane[1]) if n["notice_type"] == "tender")
    key, parent = _register(plane, service, notice, BUYER)
    artifact = corpus_artifact(CORPUS / "official-building-tender-20261026")
    changed = dataclasses.replace(artifact, raw=artifact.raw.replace(b"2026", b"2027"))
    with reqctx.use(reqctx.service("evidence")), plane[0].tx() as conn:
        plane[2].ingest(conn, changed)
    ref = _events(plane[0], "evidence.extraction.revised.v1")[-1]["data"]["revision_ref"]
    _materialize(plane, plane[3].extractions.revision(ref))
    project = _projects(plane)[0]
    with pytest.raises(StaleEnrichment):
        _apply(plane, service, key, parent)
    later = next(n for n in _facts(plane[1]) if n["id"] == notice["id"])
    assert later["version"] == 2
    key, parent = _register(plane, service, later, BUYER)
    assert _apply(plane, service, key, parent)["version"] == 3
    assert _projects(plane) == (project,)
    with reqctx.use(CTX), plane[1].tx() as conn:
        assert conn.execute(text("SELECT status FROM dom_notice_links WHERE notice_id=:n AND notice_version IN (2,3)"),
                            {"n": notice["id"]}).scalars().all() == ["pending_revision", "pending_revision"]


def test_current_all_input_rights_strictest_display_and_tamper(plane):
    service = _setup(plane)
    key, _ = _register(plane, service, _facts(plane[1])[0], BUYER)
    with reqctx.use(CTX):
        bundle = service.get(key)
        ev = bundle["outputs"][0]["evidence_ref"]
        assert service.resolve(ev).derivation["kind"] == "derivation"
        class NoRead:
            def get(self, _):
                raise AssertionError("禁止打开对象")
        for level in ("none", "citation_only"):
            restricted = _service(plane, display=lambda tags, purpose: level if "test:identity-policy" in tags else "full_text", store=NoRead())
            assert restricted.get(key) is None
            resolved = restricted.resolve(ev)
            assert resolved is None if level == "none" else resolved.derivation is None
        excerpt = _service(plane, display=lambda tags, purpose: "excerpt" if "test:identity-policy" in tags else "full_text")
        assert excerpt.get(key) is None
        assert excerpt.resolve(ev).display_level == "excerpt"
        with pytest.raises(ValueError, match="不得改"):
            service.register_reference(**REFERENCE, content={"different": True}, rights_tags=["test:identity-policy"])
        for table, column, value in (("evd_enrichments", "recipe", "'x'"), ("evd_reference_data", "version", "'2'"), ("evd_derivations", "fingerprint", "'x'")):
            with pytest.raises(Exception), plane[0].tx() as conn:
                conn.execute(text(f"UPDATE {table} SET {column}={value}"))
        with plane[0].tx() as conn:
            result_object = conn.execute(text("SELECT object_ref FROM evd_enrichments WHERE run_key=:k"), {"k": key}).scalar_one()
        result_path = plane[2].store._path(result_object)
        original_result = result_path.read_bytes()
        result_path.write_text('{}')
        with pytest.raises(ValueError, match="篡改"):
            service.get(key)
        result_path.write_bytes(original_result)
        with plane[0].tx() as conn:
            reference = conn.execute(text("SELECT object_ref FROM evd_reference_data")).scalar_one()
        path = plane[2].store._path(reference)
        original = path.read_bytes()
        path.write_text('{}')
        with pytest.raises(ValueError, match="篡改"):
            service.get(key)
        path.write_bytes(original)
        with plane[0].tx() as conn:
            derived = conn.execute(text("SELECT object_ref FROM evd_derivations")).scalar_one()
        path = plane[2].store._path(derived)
        original = path.read_bytes()
        path.write_text('{}')
        with pytest.raises(ValueError, match="篡改"):
            service.resolve(ev)
        path.write_bytes(original)
        with plane[0].tx() as conn:
            document = conn.execute(text("SELECT document_ref FROM evd_snapshots WHERE id=:s"), {"s": bundle["snapshot_ref"]}).scalar_one()
        path = plane[2].store._path(document)
        path.write_text('{}')
        with pytest.raises(ValueError):
            service.get(key)
    for ctx in (dataclasses.replace(CTX, purpose=""), dataclasses.replace(CTX, principal=reqctx.Principal("", "user"))):
        with reqctx.use(CTX):
            pass
        with pytest.raises((PermissionError, ValueError)), reqctx.use(ctx):
            service.get("missing")


def test_multi_fact_revision_is_rejected_without_enrichment_writes(plane):
    revision = _revisions(plane)[0]
    # 真实登记修订而非假造exr：复用同快照/字段证据，两个公告事实超出测试recipe事件键的能力边界。
    revision_id = ids.new("exr")
    with reqctx.use(CTX), plane[0].tx() as conn:
        conn.execute(text("""INSERT INTO evd_extraction_revisions(id,snapshot_id,extractor,pack_id,pack_version,facts,quality_status,warnings)
            VALUES (:r,:s,'multi-fixture@0.1.0','INF-PROC','1.5.0',CAST(:f AS jsonb),'ok','[]')"""),
            {"r": revision_id, "s": revision.snapshot_ref, "f": json.dumps(list(revision.facts) * 2)})
    _materialize(plane, plane[3].extractions.revision(revision_id))
    service = _service(plane)
    with reqctx.use(CTX):
        service.register_reference(**REFERENCE, content=TEST_CONTENT, rights_tags=["test:identity-policy"])
        for notice in _facts(plane[1]):
            with pytest.raises(ValueError, match="单事实"):
                service.register(notice, BUYER)
        with plane[0].tx() as conn:
            assert conn.execute(text("SELECT count(*) FROM evd_enrichments")).scalar_one() == 0
            assert conn.execute(text("SELECT count(*) FROM evd_derivations")).scalar_one() == 0
    assert _events(plane[0], "evidence.enrichment.completed.v1") == []


def test_pre_enrichment_migration_upgrade_preserves_existing_pair(plane):
    from alembic import command
    from alembic.config import Config
    from miaoqia.domain_intelligence import MIGRATIONS as DOM
    from miaoqia.evidence import MIGRATIONS as EVD
    from miaoqia.kernel import db
    configs = []
    for migrations, owner, version in ((DOM, plane[4]["owner"], "0002"),
        (EVD, plane[0].engine.url.render_as_string(hide_password=False).replace("postgresql+psycopg:", "postgresql:").replace("_app:", "_owner:"), "0001")):
        cfg = Config()
        cfg.set_main_option("script_location", migrations)
        cfg.set_main_option("sqlalchemy.url", db._url(owner))
        command.downgrade(cfg, version)
        configs.append(cfg)
    for revision in _revisions(plane):
        _materialize(plane, revision)
    notices, project, events = _facts(plane[1]), _projects(plane), _events(plane[1], "domain.fact.upserted.v1")
    for cfg in configs:
        command.upgrade(cfg, "head")
    assert _facts(plane[1]) == notices and _projects(plane) == project
    assert _events(plane[1], "domain.fact.upserted.v1") == events
    service = _service(plane)
    with reqctx.use(CTX):
        service.register_reference(**REFERENCE, content=TEST_CONTENT, rights_tags=["test:identity-policy"])
    key, parent = _register(plane, service, notices[0], BUYER)
    _apply(plane, service, key, parent)
    with pytest.raises(Exception), reqctx.use(CTX), plane[1].tx() as conn:
        conn.execute(text("DELETE FROM dom_enrichment_receipts"))
    for cfg, version in zip(configs, ("0002", "0001")):
        with pytest.raises(Exception, match="拒绝有损回退"):
            command.downgrade(cfg, version)


def test_missing_result_and_inputs_enforce_global_context_before_matching(plane):
    service = _setup(plane)
    key, parent = _register(plane, service, _facts(plane[1])[0], BUYER)
    def display(tags, purpose):
        ctx = reqctx.current()
        if ctx.tenant or ctx.principal.id != CTX.principal.id or purpose != "object_page":
            raise PermissionError("当前认证/租户/用途拒绝")
        return "none" if "test:identity-policy" in tags else "full_text"
    restricted = _service(plane, display=display)
    for context in (dataclasses.replace(CTX, tenant="tnt_" + "1" * 26, cell="cell-1"),
                    dataclasses.replace(CTX, purpose="other"),
                    dataclasses.replace(CTX, principal=reqctx.Principal("user:other", "user"))):
        with reqctx.use(context):
            for method, key_ in ((restricted.get, key), (restricted.get, "missing"), (restricted.resolve, "ev_" + "Z" * 26)):
                with pytest.raises(PermissionError):
                    method(key_)
    with reqctx.use(CTX):
        assert restricted.get(key) is None
        with pytest.raises(PermissionError):
            apply(plane[1], plane[3].extractions, restricted, key, expected_parent=parent)
        with pytest.raises(ValueError, match="未启用"):
            _service(plane, enabled=False).register(_facts(plane[1])[0], BUYER)
    assert all(n["version"] == 1 for n in _facts(plane[1]))


def test_registration_checks_real_revision_snapshot_pair(plane):
    service = _setup(plane)
    first, other = _facts(plane[1])
    forged = {**first, "snapshot_ref": other["snapshot_ref"]}
    service.facts = lambda i, v: forged  # 固定事实回调并不能替代evidence自己的真实revision归属校验。
    with reqctx.use(CTX), pytest.raises(ValueError, match="快照不一致"):
        service.register(forged, BUYER)
    assert _events(plane[0], "evidence.enrichment.completed.v1") == []
