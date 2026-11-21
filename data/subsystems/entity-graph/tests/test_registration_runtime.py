"""Real PostgreSQL owner composition. Sources, registry entries and grants are synthetic."""

from contextlib import ExitStack
from datetime import datetime, timedelta, timezone
from hashlib import sha256
import time

import pytest
from sqlalchemy import text

from miaoqia import acquisition, domain_intelligence, entity_graph, evidence
from miaoqia.acquisition.api import SourceInput
from miaoqia.acquisition.application.rights import Decisions
from miaoqia.acquisition.infrastructure.rights import Registry
from miaoqia.domain_intelligence.api import Basis
from miaoqia.domain_intelligence.api.search_projection import owner as procurement_projection
from miaoqia.domain_intelligence.application.facts import Materializer
from miaoqia.entity_graph.api.organizations import OrganizationIdentifier, RegistrationRef
from miaoqia.evidence.api import ProvenanceIntegrityError
from miaoqia.evidence.api.document_proofs import DocumentProofs
from miaoqia.evidence.application.pipeline import Artifact, Pipeline
from miaoqia.evidence.application.queries import EvidenceIntegrityError
from miaoqia.evidence.extensions.packs import PackExtractor
from miaoqia.kernel import db, ids, outbox, payloads, reqctx
from miaoqia.kernel.objectstore import FilesystemObjectStore
from miaoqia.runtime_data.organization_registry import registry_authorization
from miaoqia.runtime_data.organizations import bind_organization_search, bind_organizations
from miaoqia.runtime_data.search_projection import OwnerExports, RegistryDiscoveryPolicy, SNAPSHOT_CALLER, _CALLER
from miaoqia.sdk import ExtractionResult
from miaoqia_pack_organizations.fixtures import FIXTURE_MARKER, FixtureRegistrationExtractor, MANIFEST
from miaoqia_pack_organizations.validation import validate


SOURCE = "synthetic-enterprise-registry"
OUTSIDE = "synthetic-unapproved-registry"
USCC = OrganizationIdentifier("cn-uscc", "91000000000000000K")
NAME = "合成测试登记企业甲"
WRITER = reqctx.service("synthetic-organization-writer")
READER = reqctx.Context(reqctx.Principal("synthetic-user", "user"), "test-tenant", "test-cell", "interactive")


class SyntheticExtractor:
    id = "org.synthetic-registry"
    version = "1.0.0"

    def applies(self, doc, source):
        return source["source_id"] in (SOURCE, OUTSIDE)

    def extract(self, doc, ctx):
        name, kind, *identifiers = tuple(doc.units())
        name_span = name.span(0, len(name.text))
        fact = {"name": ctx.text(name_span), "subject": ctx.org(name_span), "enterprise_type": kind.text}
        if identifiers:
            code = identifiers[0].span(0, len(identifiers[0].text))
            proof = identifiers[-1].span(0, len(identifiers[-1].text))
            fact.update(uscc=code.text, uscc_prov=ctx.prov(proof, 1.0))
        return ExtractionResult(facts=[fact])


class WatchedStore(FilesystemObjectStore):
    reads = 0
    on_read = None

    def get(self, key, *, max_bytes=None, deadline=None):
        self.reads += 1
        if self.on_read is not None:
            callback, self.on_read = self.on_read, None
            callback()
        return super().get(key, max_bytes=max_bytes, deadline=deadline)


class SyntheticAccess:
    def __init__(self, registry):
        self.registry = registry
        self.decisions = Decisions(registry)

    def __call__(self, caller, operation, groups):
        if caller != (WRITER if operation == "confirm_registration" else READER):
            raise PermissionError("synthetic caller not admitted")
        if not groups:
            self.decisions.status()
            return lambda: bool(self.decisions.status())
        if caller == WRITER:
            # Only this synthetic unscoped writer is approved for these exact public groups.
            def current():
                with self.registry.snapshot() as snapshot:
                    states = snapshot.public_revocations(group.source_id for group in groups)
                    if any(group != SourceInput(group.source_id, (f"public:{group.source_id}",))
                           or states.get(group.source_id) is not False for group in groups):
                        raise PermissionError("synthetic registry source denied")
                    return snapshot.epoch

            epoch = current()
            return lambda: current() == epoch

        inputs = [group.dto() for group in groups]
        requests = [{"operation": op, "inputs": inputs} for op in ("fact_read", "evidence_resolve")]
        results = [self.decisions.evaluate(request) for request in requests]
        if (any(result["display"] != "full_text" for result in results)
                or len({result["epoch"] for result in results}) != 1):
            raise PermissionError("synthetic registry source denied")

        def confirm():
            return all(self.decisions.evaluate({**request, "receipt": result["receipt"]}, confirm=True)
                       ["display"] == "full_text" for request, result in zip(requests, results))

        return confirm


class Plane:
    def __init__(self, ent, dom, evd, extractions, store, rights, *, entity_dsn, evidence_dsn):
        self.ent, self.dom, self.evd = ent, dom, evd
        self.store, self.rights = store, rights
        self.extractions, self.entity_dsn, self.evidence_dsn = extractions, entity_dsn, evidence_dsn
        self.organizations = bind_organizations(ent, dom, evd, store, extractions=extractions,
                                                approved_sources={SOURCE, "fixture-r2-org"},
                                                authorize=SyntheticAccess(rights))
        self.materializer = Materializer(extractions)
        self.pipeline = Pipeline(store, [
            PackExtractor("INF-ORG", "1.1.0", FixtureRegistrationExtractor(), validate),
            # Runtime must independently reject wrong quotes, even after a faulty extractor accepts them.
            PackExtractor("INF-ORG", "1.1.0", SyntheticExtractor(), lambda *_: []),
        ])

    def ingest(self, source, key, raw, *, rights_tags=None):
        tags = (f"public:{source}",) if rights_tags is None else rights_tags
        artifact = Artifact(source, key, "text/html", raw, tags, datetime.now(timezone.utc))
        with reqctx.use(reqctx.service("evidence")), self.evd.tx() as conn:
            (revision,) = self.pipeline.ingest(conn, artifact).revisions
            event = conn.execute(text("SELECT envelope FROM kernel_outbox WHERE envelope->>'subject' = :id"),
                                 {"id": revision}).scalar_one()
        with reqctx.use(reqctx.service("domain-intelligence")), self.dom.tx() as conn:
            self.materializer.on_extraction_revised(conn, event)
            fact_id = conn.execute(text("SELECT fact_id FROM dom_fact_versions WHERE revision_ref = :id"),
                                   {"id": revision}).scalar_one()
        return RegistrationRef(fact_id, self.version(fact_id))

    def version(self, fact_id):
        with reqctx.use(reqctx.service("domain-intelligence")), self.dom.tx() as conn:
            return conn.execute(text("SELECT current_version FROM dom_facts WHERE id = :id"), {"id": fact_id}).scalar_one()

    def confirm(self, ref):
        with reqctx.use(WRITER):
            return self.organizations.confirm_registration(ref)

    def count(self):
        with reqctx.use(WRITER), self.ent.tx() as conn:
            return conn.execute(text("SELECT count(*) FROM ent_organization_versions")).scalar_one()

    def change_rights(self, action, epoch):
        change = {"action": action, "source_id": SOURCE, "expected_epoch": epoch}
        if action == "restore_public":
            change["evidence_hash"] = "sha256:" + sha256(b"synthetic restoration review").hexdigest()
        with reqctx.use(WRITER):
            return self.rights.command(change, "synthetic-operator", "synthetic registry approval exercise")


@pytest.fixture
def plane(pg_factory, tmp_path):
    with ExitStack() as stack:
        def open_owner(module):
            dsns = pg_factory()
            db.migrate(dsns["owner"], module.MIGRATIONS)
            database = db.Database(dsns["app"], pool_timeout=1.0)
            stack.callback(database.close)
            return database, dsns

        (ent, ent_dsns), (dom, _), (evd, evd_dsns), (_, acq_dsns) = (
            open_owner(module) for module in (entity_graph, domain_intelligence, evidence, acquisition)
        )
        extracts = evidence.extractions(evd_dsns["app"])
        stack.callback(extracts.close)
        rights = Registry(acq_dsns["owner"])
        stack.callback(rights.close)
        with reqctx.use(WRITER), rights.database.tx() as conn:
            for source in (SOURCE, OUTSIDE):
                conn.execute(text("INSERT INTO acq_sources (source_id, handler, access) VALUES (:source, 'test:Registry', '{}')"),
                             {"source": source})
        yield Plane(ent, dom, evd, extracts, WatchedStore(tmp_path / "objects"), rights,
                    entity_dsn=ent_dsns["app"], evidence_dsn=evd_dsns["app"])


def document(name=NAME, kind="limited_liability", code=USCC.value, extra=""):
    return (f"<p>{name}</p><p>{kind}</p>" + (f"<p>{code}</p>" if code else "") + extra).encode()


def test_search_owner_replays_confirmed_organization_and_cross_owner_revocation(plane):
    ref = plane.ingest(SOURCE, "registration:search", document())
    org = plane.confirm(ref).organization
    assert plane.confirm(ref).organization == org
    with reqctx.use(WRITER), plane.ent.tx() as conn:
        event = conn.execute(text("SELECT envelope FROM kernel_outbox WHERE topic='public.entity'")).scalar_one()
    outbox.check_topic("public.entity", event)
    payloads.validator(event["type"], {}).validate(event["data"])
    assert event["data"]["entity_id"] == org.org_id

    policy = RegistryDiscoveryPolicy(plane.rights, [SOURCE])
    access = registry_authorization(plane.rights, callers={_CALLER, SNAPSHOT_CALLER}, operation="get_entity",
                                    source_allowed=lambda source: source == SOURCE)
    organization = bind_organization_search(plane.ent, plane.dom, plane.evd, plane.store, extractions=plane.extractions,
                                            approved_sources=[SOURCE], authorize=access, policy=policy)
    owners = OwnerExports(procurement_projection(plane.dom, policy), organization)
    deadline = lambda: time.monotonic() + 4

    with reqctx.use(_CALLER):
        live = owners.prepare(event, deadline=deadline())
        live.confirm(deadline=deadline())
    handle = {"id": org.org_id, "version": org.version}
    with reqctx.use(SNAPSHOT_CALLER):
        manifest = organization.capture(page_size=1)
        assert manifest["pages"] == [[handle]]
        export = owners.prepare_snapshot_page(manifest["pages"][0], deadline=deadline())
        export.confirm(deadline=deadline())
        assert export.mutations == live.mutations == [{
            "ref": {"id": org.org_id, "version": "1", "domain": "INF-ORG", "kind": "entity"},
            "revision": 1, "withdrawn": False, "title": NAME, "identifiers": [org.org_id, USCC.value],
        }]

    plane.change_rights("revoke_public", 0)
    with reqctx.use(WRITER), plane.rights.database.tx() as conn:
        revoke = conn.execute(text("SELECT envelope FROM kernel_outbox WHERE topic='public.acquisition'")).scalar_one()
    with reqctx.use(_CALLER):
        first = owners.prepare(revoke, deadline=deadline())
        first.confirm(deadline=deadline())
        assert first.mutations == [] and first.next_after == "~org:"
        second = owners.prepare(revoke, deadline=deadline(), after=first.next_after)
        second.confirm(deadline=deadline())
        assert second.next_after is None
        assert second.mutations == [{"ref": live.mutations[0]["ref"], "revision": 2, "withdrawn": True}]


def test_current_conflict_and_recorded_history_use_fixed_owner_evidence(plane):
    a = plane.ingest(SOURCE, "registration:alpha", document())
    initial = plane.confirm(a).organization
    assert initial.registration == a and initial.identifier == USCC and initial.name == NAME
    with reqctx.use(READER):
        assert plane.organizations.resolve_entity(name=NAME).status == "candidate"
        assert plane.organizations.resolve_entity(name="仅中标公告披露的供应商").status == "unknown"
        reader = evidence.resolver(plane.evd, plane.store, display=lambda *_: "full_text")
        snapshot = reader.snapshot(initial.source.snapshot_ref)
        assert snapshot.raw == document() and snapshot.raw_sha256 == initial.source.raw_sha256
        assert [(f.path, f.value) for f in initial.fields] == [("/name/value", NAME), ("/uscc", USCC.value)]
        for field in initial.fields:
            proof = reader.resolve(field.evidence_ref)
            assert proof.snapshot_ref == snapshot.id and proof.locator and proof.snippet == field.value

    b = plane.ingest(SOURCE, "registration:alpha", document(name=NAME + "更名"))
    with reqctx.use(READER):
        assert plane.organizations.get_entity(initial.org_id, require_current=True) is None
        assert plane.organizations.get_entity(initial.org_id, version=1).organization == initial
    updated = plane.confirm(b).organization
    reverted = plane.confirm(plane.ingest(SOURCE, "registration:alpha", document())).organization
    assert [item.version for item in (initial, updated, reverted)] == [1, 2, 3]
    assert updated.org_id == reverted.org_id == initial.org_id
    assert initial.source.source_key == updated.source.source_key == reverted.source.source_key
    assert initial.source.raw_sha256 == reverted.source.raw_sha256 != updated.source.raw_sha256
    assert initial.source.snapshot_ref != reverted.source.snapshot_ref
    assert plane.confirm(a).organization == initial
    with reqctx.use(READER):
        assert plane.organizations.resolve_entity(identifier=USCC).organization == reverted
        assert plane.organizations.get_entity(initial.org_id, version=2, recorded_cutoff=initial.recorded_at) is None
        before_first = initial.recorded_at - timedelta(microseconds=1)
        assert plane.organizations.get_entity(initial.org_id, recorded_cutoff=before_first) is None

    contender = plane.ingest(SOURCE, "registration:contender", document(name="合成测试登记企业乙"))
    assert plane.confirm(contender).status == "candidate"
    with reqctx.use(READER):
        assert plane.organizations.resolve_entity(identifier=USCC).status == "unknown"
        assert plane.organizations.get_entity(initial.org_id, version=3, require_current=True) is None
        assert plane.organizations.get_entity(initial.org_id, recorded_cutoff=reverted.recorded_at).organization == reverted
        assert plane.organizations.get_entity(initial.org_id, recorded_cutoff=datetime.now(timezone.utc)) is None
    assert plane.count() == 3


def test_unapproved_fixture_weak_and_wrong_quote_cannot_confirm(plane):
    # Deliberately allowlist even the fixture source to test the independent record_scope gate.
    fixture = MANIFEST.parent / "fixture-r2-org-a/raw.html"
    raw = fixture.read_bytes()
    assert FIXTURE_MARKER in raw.decode()
    fixture_ref = plane.ingest("fixture-r2-org", "fixture:r2-org:fixture-r2-org-a", raw)
    outside = plane.ingest(OUTSIDE, "registration:outside", document())
    for ref in (fixture_ref, outside, RegistrationRef(ids.new("ogr"), 1)):
        with pytest.raises(PermissionError):
            plane.confirm(ref)
    with pytest.raises(ValueError, match="invalid ogr"):
        plane.confirm(RegistrationRef(ids.new("ntc"), 1))
    assert plane.store.reads == 0

    branch = plane.ingest(SOURCE, "registration:branch", document(kind="branch"))
    weak = plane.ingest(SOURCE, "registration:weak", document(code=None))
    unspecified = plane.ingest(SOURCE, "registration:unknown-kind", document(kind="other"))
    for ref in (branch, weak, unspecified):
        assert plane.confirm(ref).status == "candidate"
    wrong = plane.ingest(SOURCE, "registration:wrong-proof", document(extra="<p>同一快照中的其他字段</p>"))
    with pytest.raises(ProvenanceIntegrityError, match="locator, quote hash or field value"):
        plane.confirm(wrong)
    assert plane.count() == 0


def test_revocation_during_proof_rolls_back_and_applies_to_pinned_reads(plane):
    ref = plane.ingest(SOURCE, "registration:alpha", document())

    def revoke_during_read():
        assert plane.change_rights("revoke_public", 0) == 1

    plane.store.on_read = revoke_during_read
    with pytest.raises(PermissionError):
        plane.confirm(ref)
    assert plane.count() == 0
    restored = plane.change_rights("restore_public", 1)
    assert restored == 2
    confirmed = plane.confirm(ref).organization
    assert plane.change_rights("revoke_public", restored) == 3
    last_reads = plane.store.reads
    with reqctx.use(READER):
        with pytest.raises(PermissionError):
            plane.organizations.resolve_entity(identifier=USCC)
        for scope in ({"version": 1}, {"recorded_cutoff": confirmed.recorded_at}, {"require_current": True}):
            with pytest.raises(PermissionError):
                plane.organizations.get_entity(confirmed.org_id, **scope)
    assert plane.store.reads == last_reads
    assert plane.count() == 1


@pytest.mark.parametrize("kind", ["raw", "normalized"])
def test_object_tampering_invalidates_the_pinned_proof(plane, kind):
    ref = plane.ingest(SOURCE, "registration:alpha", document())
    original = plane.confirm(ref).organization
    if kind == "raw":
        raw_key = original.source.raw_sha256.removeprefix("sha256:")
        (plane.store.root / "raw" / raw_key).write_bytes(b"tampered")
        expected = ProvenanceIntegrityError
    else:
        (path,) = (plane.store.root / "normalized").rglob(f"{original.source.snapshot_ref}.json")
        path.write_text("{}")
        expected = EvidenceIntegrityError
    with reqctx.use(READER), pytest.raises(expected):
        plane.organizations.get_entity(original.org_id, version=original.version)
    assert plane.count() == 1


def test_equal_bytes_and_values_cannot_substitute_another_snapshots_handle(plane):
    first = plane.ingest(SOURCE, "registration:alpha", document())
    initial = plane.confirm(first).organization
    other = plane.ingest(SOURCE, "registration:other", document())
    with reqctx.use(READER):
        facts = domain_intelligence.queries(plane.dom, allowed=lambda *_: True)
        alternate = facts.get_fact(other.fact_id, Basis("current"), version=other.version).fact
        alternate_ref = alternate["name"]["prov"]["evidence_ref"]
        proofs = DocumentProofs(plane.evd, plane.store)
        assert proofs.source(alternate["snapshot_ref"]).raw_sha256 == initial.source.raw_sha256
        assert alternate_ref != initial.fields[0].evidence_ref
        with pytest.raises(ProvenanceIntegrityError, match="fixed snapshot"):
            proofs.requirements(initial.source.snapshot_ref, (alternate_ref,))
