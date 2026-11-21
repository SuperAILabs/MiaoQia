"""Public organization search against migrated PostgreSQL and synthetic registries."""

from dataclasses import dataclass, replace
from datetime import datetime
import time

import pytest
from sqlalchemy import event, text

from miaoqia.acquisition.api import SourceInput
from miaoqia.entity_graph import public_organizations
from miaoqia.entity_graph.api.organizations import OrganizationIdentifier
from miaoqia.entity_graph.api.search_projection import ProjectionUnavailable, owner
from miaoqia.entity_graph.domain.organizations import _USCC_ALPHABET, _USCC_WEIGHTS
from miaoqia.kernel import ids, reqctx
from miaoqia.sdk.derivation import fingerprint

from test_public_organizations import READER, TEST_USCC, authority, ingest  # noqa: F401
from test_registration_runtime import SOURCE as REGISTRY_SOURCE, document, plane  # noqa: F401

SEARCH = reqctx.service("synthetic-org-search")
SOURCE = "synthetic-registry"


@dataclass(frozen=True)
class Decision:
    epoch: int
    approved: frozenset[str]


class Discovery:
    epoch = 1
    approved = frozenset({SOURCE})
    request_allowed = True
    receipt_valid = True

    def __init__(self):
        self.tags = {}
        self.admitted = []

    def resolve(self, sources, *, deadline):
        assert reqctx.current() == SEARCH and time.monotonic() < deadline
        return Decision(self.epoch, sources & self.approved)

    def admit(self, operation, refs):
        assert operation == "get_entity" and refs == () and reqctx.current() == SEARCH
        return lambda: reqctx.get() == SEARCH and self.request_allowed

    def admit_public(self, ref, source_id, *, deadline):
        assert reqctx.current() == SEARCH and time.monotonic() < deadline
        self.admitted.append(ref)
        required = (f"public:{source_id}",)
        if self.tags.get(ref, required) != required:
            return None
        return lambda: (reqctx.get() == SEARCH and self.receipt_valid
                        and self.tags.get(ref, required) == required)


@pytest.fixture
def projection(authority):
    database, registrations, bind = authority
    discovery = Discovery()
    trusted = public_organizations(database, registrations=registrations, evidence=registrations, admit=discovery.admit)
    return database, registrations, bind, discovery, owner(trusted, discovery, admit_public=discovery.admit_public)


def _deadline():
    return time.monotonic() + 9


def _handle(org):
    return {"id": org.org_id, "version": org.version}


def _mutation(org, revision, *, withdrawn=False):
    dto = {"ref": {"kind": "entity", "domain": "INF-ORG", "id": org.org_id, "version": str(org.version)},
           "revision": revision, "withdrawn": withdrawn}
    if not withdrawn:
        dto.update(title=org.name, identifiers=[org.org_id, org.identifier.value])
    return dto


def _apply(prepare, *args, **kwargs):
    with reqctx.use(SEARCH):
        prepared = prepare(*args, **kwargs, deadline=_deadline())
        prepared.confirm(deadline=_deadline())
        return prepared


def _synthetic_uscc(index):
    body = f"910000{index:011d}"
    total = sum(_USCC_ALPHABET.index(char) * weight for char, weight in zip(body, _USCC_WEIGHTS, strict=True))
    return OrganizationIdentifier("cn-uscc", body + _USCC_ALPHABET[-total % 31])


def test_exact_page_replay_and_unconfirmed_registration_version(projection):
    _, registrations, bind, _, search = projection
    first = registrations.add()
    org1 = ingest(bind, first).organization
    assert _apply(search.prepare_ids, [org1.org_id]).mutations == [_mutation(org1, 1)]
    assert _apply(search.prepare_snapshot_page, [_handle(org1)]).mutations == [_mutation(org1, 1)]

    with reqctx.use(SEARCH):
        prepared = search.prepare_ids([org1.org_id], deadline=_deadline())
        newer = registrations.add(previous=first, name="合成登记更名")
        with pytest.raises(ProjectionUnavailable, match="current registration changed"):
            prepared.confirm(deadline=_deadline())
    assert _apply(search.prepare_registration, first.ref.fact_id).mutations == [_mutation(org1, 2, withdrawn=True)]
    assert _apply(search.prepare_registration, first.ref.fact_id).mutations == [_mutation(org1, 2, withdrawn=True)]

    org2 = ingest(bind, newer).organization
    assert _apply(search.prepare_registration, first.ref.fact_id).mutations == [_mutation(org2, 3)]
    assert _apply(search.prepare_snapshot_page, [_handle(org2)]).mutations == [_mutation(org2, 3)]
    with reqctx.use(SEARCH):
        with pytest.raises(ProjectionUnavailable, match="snapshot page no longer matches"):
            search.prepare_snapshot_page([_handle(org1)], deadline=_deadline())
        prepared = search.prepare_ids([org2.org_id], deadline=_deadline())
        del registrations.heads[first.ref.fact_id]
        with pytest.raises(ProjectionUnavailable, match="current registration changed"):
            prepared.confirm(deadline=_deadline())
    assert _apply(search.prepare_registration, first.ref.fact_id).mutations == [_mutation(org2, 4, withdrawn=True)]


def test_conflict_uses_affected_org_id_and_emits_only_tombstone(projection):
    _, registrations, bind, _, search = projection
    org = ingest(bind, registrations.add()).organization
    assert _apply(search.prepare_ids, [org.org_id]).mutations == [_mutation(org, 1)]
    with reqctx.use(SEARCH):
        prepared = search.prepare_ids([org.org_id], deadline=_deadline())
        contender = registrations.add()
        assert ingest(bind, contender).status == "candidate"
        with pytest.raises(ProjectionUnavailable, match="organization or projection revision changed"):
            prepared.confirm(deadline=_deadline())

    assert _apply(search.prepare_registration, contender.ref.fact_id).mutations == []
    assert _apply(search.prepare_ids, [org.org_id]).mutations == [_mutation(org, 2, withdrawn=True)]
    assert _apply(search.prepare_snapshot_page, [_handle(org)]).mutations == [_mutation(org, 2, withdrawn=True)]


def test_source_pagination_revocation_recovery_and_rights_floor(projection):
    _, registrations, bind, policy, search = projection
    organizations = sorted((ingest(bind, registrations.add(identifier=_synthetic_uscc(n))).organization
                            for n in range(33)), key=lambda org: org.org_id)
    assert _apply(search.prepare_ids, [org.org_id for org in reversed(organizations[:32])]).mutations == [
        _mutation(org, 1) for org in organizations[:32]
    ]
    assert _apply(search.prepare_ids, [organizations[-1].org_id]).mutations == [_mutation(organizations[-1], 1)]

    with reqctx.use(SEARCH):
        with pytest.raises(ProjectionUnavailable, match="has not caught up"):
            search.prepare_source(SOURCE, rights_version=2, deadline=_deadline())
        pending = search.prepare_ids([organizations[0].org_id], deadline=_deadline())
        policy.epoch = 2
        policy.approved = frozenset()
        with pytest.raises(ProjectionUnavailable, match="source rights changed"):
            pending.confirm(deadline=_deadline())

    admitted = list(policy.admitted)
    first = _apply(search.prepare_source, SOURCE, rights_version=2)
    assert first.next_after == organizations[31].org_id
    assert first.mutations == [_mutation(org, 2, withdrawn=True) for org in organizations[:32]]
    last = _apply(search.prepare_source, SOURCE, after=first.next_after, rights_version=2)
    assert last.mutations == [_mutation(organizations[-1], 2, withdrawn=True)] and last.next_after is None
    assert admitted == policy.admitted

    policy.epoch = 3
    policy.approved = frozenset({SOURCE})
    first = _apply(search.prepare_source, SOURCE, rights_version=3)
    assert first.mutations == [_mutation(org, 3) for org in organizations[:32]]
    last = _apply(search.prepare_source, SOURCE, after=first.next_after, rights_version=3)
    assert last.mutations == [_mutation(organizations[-1], 3)] and last.next_after is None
    assert _apply(search.prepare_source, "different-source", rights_version=3).mutations == []


def test_pure_public_admission_and_proof_are_required(projection):
    _, registrations, bind, discovery, search = projection
    reg = registrations.add()
    org = ingest(bind, reg).organization
    other = ingest(bind, registrations.add(identifier=OrganizationIdentifier("test-registry", "not-uscc"))).organization
    discovery.tags[reg.ref] = (f"public:{SOURCE}", "private:fixture")
    assert _apply(search.prepare_ids, [org.org_id, other.org_id]).mutations == []
    assert discovery.admitted == [reg.ref, reg.ref]
    with reqctx.use(SEARCH):
        denied = search.prepare_ids([org.org_id], deadline=_deadline())
        del discovery.tags[reg.ref]
        with pytest.raises(ProjectionUnavailable, match="admission changed"):
            denied.confirm(deadline=_deadline())

        proof_key = reg.ref, reg.name.path, reg.name.evidence_ref
        proof = registrations.proofs[proof_key]
        registrations.proofs[proof_key] = replace(proof, source=replace(proof.source, snapshot_ref=ids.new("snp")))
        with pytest.raises(ProjectionUnavailable, match="proof is inconsistent"):
            search.prepare_ids([org.org_id], deadline=_deadline())
        registrations.proofs[proof_key] = proof
        prepared = search.prepare_ids([org.org_id], deadline=_deadline())
        assert prepared.mutations == [_mutation(org, 1)]
        discovery.receipt_valid = False
        with pytest.raises(PermissionError, match="denied or expired"):
            prepared.confirm(deadline=_deadline())
    with reqctx.use(READER), pytest.raises(PermissionError, match="public service"):
        search.prepare_ids([org.org_id], deadline=_deadline())


def test_capture_pins_all_current_versions_before_later_commit(projection):
    database, registrations, bind, _, search = projection
    with reqctx.use(SEARCH):
        assert search.capture()["pages"] == []
    reg = registrations.add()
    first = ingest(bind, reg).organization
    second = ingest(bind, registrations.add(identifier=_synthetic_uscc(1))).organization
    third_reg = registrations.add(identifier=_synthetic_uscc(2))
    with database.engine.connect() as conn:
        before = conn.execute(text("SELECT clock_timestamp()")).scalar_one()
    after_commit = None

    def advance(conn, cursor, statement, parameters, context, executemany):
        nonlocal after_commit
        if "WITH basis AS MATERIALIZED" in statement:
            newer = registrations.add(previous=reg, name="合成甲新名")
            assert ingest(bind, newer).organization.version == 2
            assert ingest(bind, third_reg).status == "confirmed"
            with database.engine.connect() as observer:
                after_commit = observer.execute(text("SELECT clock_timestamp()")).scalar_one()

    event.listen(database.engine, "after_cursor_execute", advance)
    try:
        with reqctx.use(SEARCH):
            manifest = search.capture(page_size=1, max_handles=2)
    finally:
        event.remove(database.engine, "after_cursor_execute", advance)

    ordered = sorted((first, second), key=lambda org: org.org_id)
    assert manifest["pages"] == [[_handle(org)] for org in ordered]
    assert manifest["handle_count"] == 2 and before <= datetime.fromisoformat(manifest["as_of"]) <= after_commit
    assert manifest["snapshot_id"] == fingerprint({k: v for k, v in manifest.items() if k != "snapshot_id"})
    with reqctx.use(SEARCH):
        newer = search.capture(page_size=1)
        assert newer["handle_count"] == 3 and [h for page in newer["pages"] for h in page if h["id"] == first.org_id] == [
            {"id": first.org_id, "version": 2}
        ]
        with pytest.raises(ProjectionUnavailable, match="exceeds handle limit"):
            search.capture(max_handles=2)


def test_scope_and_invalid_inputs_fail_closed(projection):
    _, registrations, bind, discovery, search = projection
    reg = registrations.add(identifier=TEST_USCC)
    org = ingest(bind, reg).organization
    with reqctx.use(SEARCH):
        with pytest.raises(ProjectionUnavailable, match="version unavailable"):
            search.prepare_ids([ids.new("org")], deadline=_deadline())
        with pytest.raises(ValueError, match="duplicate"):
            search.prepare_ids([org.org_id, org.org_id], deadline=_deadline())
        with pytest.raises(ValueError, match="invalid version"):
            search.prepare_snapshot_page([{"id": org.org_id, "version": True}], deadline=_deadline())
        with pytest.raises(ValueError, match="invalid org identifier"):
            search.prepare_source(SOURCE, after=reg.ref.fact_id, rights_version=1, deadline=_deadline())
        with pytest.raises(ProjectionUnavailable, match="deadline"):
            search.prepare_ids([org.org_id], deadline=time.monotonic() - 1)
        discovery.request_allowed = False
        with pytest.raises(PermissionError, match="denied or expired"):
            search.capture()


def test_production_owner_ports_with_synthetic_documents_and_registry(plane, monkeypatch):
    ref = plane.ingest(REGISTRY_SOURCE, "synthetic:public-search", document())
    authority = plane.organizations
    org = plane.confirm(ref).organization

    class RegistryPolicy:
        def resolve(self, source_ids, *, deadline):
            assert time.monotonic() < deadline
            with plane.rights.snapshot() as snapshot:
                states = snapshot.public_revocations(source_ids)
                return Decision(snapshot.epoch, frozenset(s for s, revoked in states.items() if revoked is False))

    policy = RegistryPolicy()

    def authorize(caller, operation, groups):
        assert caller == SEARCH and operation == "get_entity"
        if not groups:
            return lambda: reqctx.get() == SEARCH
        assert groups == (SourceInput(REGISTRY_SOURCE, (f"public:{REGISTRY_SOURCE}",)),)

        def rights():
            return policy.resolve(frozenset({REGISTRY_SOURCE}), deadline=_deadline())

        decision = rights()
        assert decision.approved == {REGISTRY_SOURCE}
        return lambda: reqctx.get() == SEARCH and rights() == decision

    monkeypatch.setattr(authority.registrations, "authorize", authorize)

    def admit_public(ref, source_id, *, deadline):
        assert source_id == REGISTRY_SOURCE and time.monotonic() < deadline
        return authority.admit("get_entity", (ref,))

    search = owner(authority, policy, admit_public=admit_public)
    assert _apply(search.prepare_ids, [org.org_id]).mutations == [_mutation(org, 1)]
    assert plane.store.reads > 0
    reads = plane.store.reads

    revoke_epoch = plane.change_rights("revoke_public", 0)
    assert _apply(search.prepare_source, REGISTRY_SOURCE, rights_version=revoke_epoch).mutations == [
        _mutation(org, 2, withdrawn=True)
    ]
    assert plane.store.reads == reads
    restore_epoch = plane.change_rights("restore_public", revoke_epoch)
    assert _apply(search.prepare_source, REGISTRY_SOURCE, rights_version=restore_epoch).mutations == [_mutation(org, 3)]
    assert plane.store.reads > reads
