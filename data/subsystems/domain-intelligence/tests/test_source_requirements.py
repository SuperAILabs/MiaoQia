"""Grouped owner provenance reuses the real-PG pipeline and enrichment harness."""
import dataclasses
from concurrent.futures import ThreadPoolExecutor
import pytest
from sqlalchemy import text
from sqlalchemy.pool import QueuePool
from miaoqia import domain_intelligence, evidence
from miaoqia.acquisition.api import SourceInput
from miaoqia.domain_intelligence.api import Basis
from miaoqia.domain_intelligence.application import requirements
from miaoqia.evidence.api import Closure, Unbound
from miaoqia.evidence.replay import corpus_artifact
from miaoqia.kernel import reqctx
from test_facts import CORPUS, _facts, plane  # noqa: F401
from test_projects import _materialize, _projects, _revisions
from test_enrichments import CTX, BUYER, _setup, _register, _apply


def test_complete_same_source_groups_and_project_any_member_denial(plane):
    for revision in _revisions(plane):
        _materialize(plane, revision)
    notices, project = _facts(plane[1]), _projects(plane)[0]
    seen = []
    provenance = evidence.source_requirements(plane[0])
    query = domain_intelligence.queries(plane[1], authorize=lambda groups, purpose: seen.append(groups) or True, provenance=provenance)
    with reqctx.use(CTX):
        assert query.get_fact(project["id"], Basis("current")).fact == project
        closure = query.requirements(project["id"], project["version"])
        assert len(closure.sources) == 2 and len({g.source_id for g in closure.sources}) == 1
        assert {g.tags for g in closure.sources} == {tuple(sorted(n["rights_tags"])) for n in notices}
        denied = domain_intelligence.queries(plane[1], authorize=lambda groups, purpose: all(
            "corpus:official-building-award-20261026" not in g.tags for g in groups), provenance=provenance)
        assert denied.get_fact(project["id"], Basis("current")) is None
    assert len(seen[-1]) == 2


@pytest.mark.parametrize("source,tags", [("legacy source/合法", ["local"]), ("legacy", [f"tag:{i}" for i in range(17)])])
def test_local_pipeline_and_callback_accept_non_wire_provenance(plane, source, tags):
    art = dataclasses.replace(corpus_artifact(CORPUS / "official-award-20261014"), source_id=source, rights_tags=tags)
    with reqctx.use(CTX), plane[0].tx() as conn:
        result = plane[2].ingest(conn, art)
    revision = plane[3].extractions.revision(result.revisions[0])
    _materialize(plane, revision)
    fact, calls = _facts(plane[1])[0], []
    with reqctx.use(CTX):
        legacy = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
        assert legacy.get_fact(fact["id"], Basis("current")).fact == fact
        assert legacy.requirements(fact["id"], 1).sources == (SourceInput(source, tuple(sorted(tags))),)
        remote = domain_intelligence.queries(plane[1], authorize=lambda *args: calls.append(args) or True, provenance=evidence.source_requirements(plane[0]))
        assert remote.get_fact(fact["id"], Basis("current")) is None
    assert not calls


def test_more_than_64_groups_preserved_on_write_and_remote_denied(plane, monkeypatch):
    closure = Closure.merge(Closure(tuple(SourceInput("source", (f"tag:{i}",)) for i in range(65))))
    monkeypatch.setattr(requirements, "base", lambda revision: closure)
    _materialize(plane, _revisions(plane)[0])
    fact = _facts(plane[1])[0]
    with reqctx.use(CTX):
        local = domain_intelligence.queries(plane[1], allowed=lambda tags, purpose: True)
        assert local.get_fact(fact["id"], Basis("current")).fact == fact
        assert local.requirements(fact["id"], 1).sources == closure.sources
        with plane[1].tx() as conn:
            assert requirements.read(conn, fact) == closure
        with pytest.raises(Unbound, match="unsupported"):
            evidence.source_requirements(plane[0]).resolve(closure)


def test_identity_reference_legacy_usable_remote_unbound(plane):
    service = _setup(plane)
    notice = _facts(plane[1])[0]
    key, parent = _register(plane, service, notice, BUYER)
    result = _apply(plane, service, key, parent)
    calls = []
    with reqctx.use(CTX):
        assert service.get(key)
        assert service.requirements(key).references[0].name == "test-identity-policy"
        remote = evidence.enrichments(plane[0], plane[2].store, facts=service.facts, fact_requirements=service.fact_requirements, authorize=lambda *args: calls.append(args) or "full_text", enable_test_recipe=True)
        assert remote.get(key) is None
        assert remote.resolve(service.get(key)["outputs"][0]["evidence_ref"]) is None
        query = domain_intelligence.queries(plane[1], authorize=lambda *args: True, provenance=evidence.source_requirements(plane[0]))
        assert query.get_fact(result["notice_id"], Basis("current")) is None
        assert query.get_fact(notice["id"], Basis("current"), version=1).fact == notice
    assert not calls


def test_structured_queries_reuse_one_domain_connection(plane):
    _materialize(plane, _revisions(plane)[0])
    fact = _facts(plane[1])[0]
    plane[1].engine.dispose()
    plane[1].engine.pool = QueuePool(plane[1].engine.pool._creator, pool_size=1, max_overflow=0, timeout=.3)
    query = domain_intelligence.queries(plane[1], authorize=lambda groups, purpose: True, provenance=evidence.source_requirements(plane[0]))
    def read(_):
        with reqctx.use(CTX):
            return query.get_fact(fact["id"], Basis("current")).fact
    with ThreadPoolExecutor(max_workers=4) as executor:
        assert all(f == fact for f in executor.map(read, range(8)))


def test_writer_closure_failure_rolls_back_fact_and_outbox(plane, monkeypatch):
    revision = _revisions(plane)[0]
    def fail(*args, **kwargs):
        raise ValueError("closure fault")
    monkeypatch.setattr(requirements, "put", fail)
    with pytest.raises(ValueError, match="closure fault"):
        _materialize(plane, revision)
    assert _facts(plane[1]) == []
    with reqctx.use(CTX), plane[1].tx() as conn:
        assert conn.execute(text("SELECT count(*) FROM kernel_outbox")).scalar_one() == 0


def test_modes_explicit_and_neither_denies(plane):
    with pytest.raises(ValueError):
        domain_intelligence.queries(plane[1], allowed=lambda *a: True, authorize=lambda *a: True)
    with pytest.raises(ValueError):
        evidence.resolver(plane[0], plane[2].store, display=lambda *a: "full_text", authorize=lambda *a: "full_text")
    with pytest.raises(ValueError):
        evidence.enrichments(plane[0], plane[2].store, facts=lambda *a: None, display=lambda *a: "full_text", authorize=lambda *a: "full_text")
    with reqctx.use(CTX), pytest.raises(PermissionError):
        domain_intelligence.queries(plane[1]).get_fact("missing", Basis("current"))


def _owner_databases(plane):
    from miaoqia.kernel import db
    evd_dsn = plane[0].engine.url.render_as_string(hide_password=False).replace("postgresql+psycopg:", "postgresql:").replace("_app:", "_owner:")
    return db.Database(evd_dsn), db.Database(plane[4]["owner"])


def _legacy_without_sidecars(evd, dom):
    # Simulate a pre-foundation database after upgrade; subject objects/facts stay untouched.
    with reqctx.use(CTX):
        for database, table in ((evd, "evd_requirement_sets"), (dom, "dom_version_requirements")):
            with database.tx() as conn:
                conn.execute(text("ALTER TABLE "+table+" DISABLE TRIGGER USER"))
                conn.execute(text("DELETE FROM "+table))
                conn.execute(text("ALTER TABLE "+table+" ENABLE TRIGGER USER"))


def test_explicit_owner_backfill_is_atomic_idempotent_and_preserves_old_bytes(plane, monkeypatch):
    service = _setup(plane)
    original = _facts(plane[1])[0]
    key, parent = _register(plane, service, original, BUYER)
    _apply(plane, service, key, parent)
    facts_before, projects_before = _facts(plane[1]), _projects(plane)
    evd, dom = _owner_databases(plane)
    try:
        _legacy_without_sidecars(evd, dom)
        owner = evidence.enrichments(evd, plane[2].store, facts=service.facts, fact_requirements=service.fact_requirements,
                                    display=lambda *args: "full_text", enable_test_recipe=True)
        with reqctx.use(CTX):
            assert service.get(key)  # legacy callback is independent of sidecar availability
            with pytest.raises(Unbound):
                service.requirements(key)
            with pytest.raises(PermissionError):
                service.backfill([key], actor="reviewer", reason="requires owner")
            with pytest.raises(PermissionError):
                owner.backfill([key, "sha256:"+"f"*64], actor="reviewer", reason="whole batch")
            with evd.tx() as conn:
                assert conn.execute(text("SELECT count(*) FROM evd_requirement_sets")).scalar_one() == 0
            domain_intelligence.backfill_requirements(dom, plane[3].extractions, owner, [[original["id"], original["version"]]],
                actor="reviewer", reason="verify source base first")
            from miaoqia.evidence.application import provenance as stored
            actual_put, calls = stored.put, []
            def fail_second(*args, **kwargs):
                actual_put(*args, **kwargs)
                calls.append(1)
                if len(calls) == 2:
                    raise ValueError("backfill transaction fault")
            with monkeypatch.context() as patch:
                patch.setattr(stored, "put", fail_second)
                with pytest.raises(ValueError, match="transaction fault"):
                    owner.backfill([key], actor="reviewer", reason="fault after both inserts")
            with evd.tx() as conn:
                assert conn.execute(text("SELECT count(*) FROM evd_requirement_sets")).scalar_one() == 0
            assert owner.backfill([key], actor="reviewer", reason="verify frozen history") == 2
            assert owner.backfill([key], actor="retry", reason="exact retry") == 2
            def retry(_):
                with reqctx.use(CTX):
                    return owner.backfill([key], actor="concurrent", reason="same proof")
            with ThreadPoolExecutor(max_workers=3) as pool:
                assert list(pool.map(retry, range(6))) == [2]*6
            handles = [[p["id"], p["version"]] for p in projects_before]
            assert domain_intelligence.backfill_requirements(dom, plane[3].extractions, owner, handles,
                actor="reviewer", reason="verify fixed dependency closure") >= 3
            assert domain_intelligence.backfill_requirements(dom, plane[3].extractions, owner, handles,
                actor="retry", reason="exact retry") >= 3
            with evd.tx() as conn:
                assert set(conn.execute(text("SELECT actor,db_actor FROM evd_requirement_sets")).all()) == {("reviewer", evd.engine.url.username)}
            with dom.tx() as conn:
                assert set(conn.execute(text("SELECT actor,db_actor FROM dom_version_requirements")).all()) == {("reviewer", dom.engine.url.username)}
        assert _facts(plane[1]) == facts_before and _projects(plane) == projects_before
    finally:
        evd.close()
        dom.close()


def test_binding_availability_is_not_saved_in_closure_and_citation_reads_no_objects(plane):
    recipe = evidence.load_recipe(CORPUS.parents[2] / "industries/building-materials",
        "miaoqia_pack_building_materials.topic:build_recipe", CORPUS.parents[2])
    service = _setup(plane, recipes=[recipe])
    fact = next(n for n in _facts(plane[1]) if n["notice_type"] == "tender")
    key, parent = _register(plane, service, fact, recipe.id)
    with reqctx.use(CTX):
        closure = service.requirements(key)
        provenance = evidence.source_requirements(plane[0])
        with pytest.raises(Unbound, match="unbound"):
            provenance.resolve(closure)
    evd, dom = _owner_databases(plane)
    try:
        with reqctx.use(CTX), evd.tx() as conn:
            conn.execute(text("SELECT evd_provenance.bind_reference('proc-building-topic','0.1.0','reviewer','fixed reviewed reference')"))
        class NoObjects:
            def get(self, *args):
                raise AssertionError("citation/denial opened content")
        with reqctx.use(CTX):
            assert service.requirements(key) == closure
            groups = provenance.resolve(closure)
            assert any(g.source_id == "reference-proc-building-topic-v0-1" for g in groups)
            assert len(groups) >= 2
            remote = evidence.enrichments(plane[0], NoObjects(), facts=service.facts, fact_requirements=service.fact_requirements,
                authorize=lambda groups, purpose: "citation_only", recipes=[recipe])
            ref_id = service.get(key)["outputs"][0]["evidence_ref"]
            assert remote.resolve(ref_id).display_level == "citation_only"
            document_ref = fact["title"]["prov"]["evidence_ref"]
            assert evidence.resolver(plane[0], NoObjects(), authorize=lambda *args: "citation_only").resolve(document_ref).display_level == "citation_only"
            assert evidence.resolver(plane[0], NoObjects(), authorize=lambda *args: "none").resolve(document_ref) is None
            full = evidence.enrichments(plane[0], plane[2].store, facts=service.facts, fact_requirements=service.fact_requirements,
                authorize=lambda groups, purpose: "full_text", recipes=[recipe])
            assert full.get(key) == service.get(key)
            after_binding = next(n for n in _facts(plane[1]) if n["notice_type"] == "award")
            other_key, _ = _register(plane, service, after_binding, recipe.id)
            assert full.get(other_key) == service.get(other_key)
            # An approved binding cannot cure a corrupt immutable requirement proof.
            with evd.tx() as conn:
                conn.execute(text("ALTER TABLE evd_requirement_sets DISABLE TRIGGER USER"))
                conn.execute(text("UPDATE evd_requirement_sets SET subject_hash=:bad WHERE kind='derivation' AND key=:id"),
                             {"bad": "sha256:"+"0"*64, "id": ref_id})
                conn.execute(text("ALTER TABLE evd_requirement_sets ENABLE TRIGGER USER"))
            from miaoqia.evidence.api import ProvenanceIntegrityError
            with pytest.raises(ProvenanceIntegrityError):
                remote.resolve(ref_id)
    finally:
        evd.close()
        dom.close()


def test_evidence_inherits_hidden_parent_reference_requirements(plane):
    from miaoqia.evidence.api import Recipe
    from miaoqia.evidence.application.enrichments import TEST_CONTENT, identity, identity_outputs
    recipes = [Recipe("local."+name+"@1", "/buyer/name_raw", "test.identity@0.1.0",
        {"name": name, "version": "1"}, TEST_CONTENT, identity, identity_outputs, True)
        for name in ("first-policy", "second-policy", "third-policy")]
    service = _setup(plane, recipes=recipes)
    notice = _facts(plane[1])[0]
    for recipe in recipes[:2]:
        key, parent = _register(plane, service, notice, recipe.id)
        _apply(plane, service, key, parent)
    current = next(n for n in _facts(plane[1]) if n["id"] == notice["id"])
    with reqctx.use(CTX):
        parent = domain_intelligence.queries(plane[1], allowed=lambda *args: True).requirements(current["id"], current["version"])
        assert {r.name for r in parent.references} == {"first-policy", "second-policy"}
        # Current JSON points only at second-policy's derivative; first-policy survives in owner metadata.
        key = service.register(current, recipes[2].id)
        closure = service.requirements(key)
        assert {r.name for r in closure.references} == {"first-policy", "second-policy", "third-policy"}
        assert service.get(key)


def test_owner_fact_proof_binds_exact_subject_hash_and_missing_proof_stays_local(plane):
    import copy
    from miaoqia.evidence.api import ProvenanceIntegrityError
    service = _setup(plane)
    original = _facts(plane[1])[0]
    forged = copy.deepcopy(original)
    forged["buyer"]["name_raw"] = "forged unrelated field"
    actual_facts = service.facts
    service.facts = lambda *args: forged
    with reqctx.use(CTX), pytest.raises(ProvenanceIntegrityError, match="owner proof"):
        service.register(forged, BUYER)
    service.facts = actual_facts
    with reqctx.use(CTX):
        legacy = evidence.enrichments(plane[0], plane[2].store, facts=service.facts,
                                      display=lambda *args: "full_text", enable_test_recipe=True)
        key = legacy.register(original, BUYER)
        assert legacy.get(key)
        with pytest.raises(Unbound):
            legacy.requirements(key)
        remote = evidence.enrichments(plane[0], plane[2].store, facts=service.facts,
            fact_requirements=service.fact_requirements, authorize=lambda *args: "full_text", enable_test_recipe=True)
        assert remote.get(key) is None
        next_key = service.register(original, "test.buyer-provenance@0.1.1")
        assert service.get(next_key)
        with pytest.raises(Unbound):
            service.requirements(next_key)
        with plane[0].tx() as conn:
            assert conn.execute(text("SELECT count(*) FROM evd_requirement_sets")).scalar_one() == 0


def test_owner_metadata_is_immutable_and_fixed_port_requires_a_version(plane):
    service = _setup(plane)
    notice = _facts(plane[1])[0]
    _register(plane, service, notice, BUYER)
    evd, dom = _owner_databases(plane)
    try:
        with reqctx.use(CTX):
            query = domain_intelligence.queries(plane[1], allowed=lambda *args: True)
            with pytest.raises(ValueError, match="fixed positive version"):
                query.requirements(notice["id"], None)
            for database, table in ((evd, "evd_requirement_sets"), (dom, "dom_version_requirements")):
                for statement in ("UPDATE "+table+" SET actor='forged'", "DELETE FROM "+table, "TRUNCATE "+table):
                    with pytest.raises(Exception), database.tx() as conn:
                        conn.execute(text(statement))
    finally:
        evd.close()
        dom.close()
