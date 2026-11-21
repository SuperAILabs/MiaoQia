"""Public organization authority with real PostgreSQL and synthetic owner ports."""
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from datetime import datetime, timedelta
from threading import Barrier

import pytest
from sqlalchemy import text
from sqlalchemy.exc import DBAPIError

from miaoqia.entity_graph import MIGRATIONS, public_organizations
from miaoqia.entity_graph.api.organizations import (
    EvidenceField,
    OrganizationIdentifier,
    OrganizationProofError,
    Registration,
    RegistrationEvidence,
    RegistrationRef,
    RegistrationSource,
)
from miaoqia.kernel import db, ids, reqctx

WRITER = reqctx.service("organization-test-writer")
READER = reqctx.Context(reqctx.Principal("test-user", "user"), "test-tenant", "test-cell", "interactive")

# Test-only input with a computed checksum. No real registry identity is claimed.
TEST_USCC = OrganizationIdentifier("cn-uscc", "91000000000000000K")
NAMESPACE = "synthetic-registry"


class Owners:
    def __init__(self):
        self.facts = {}
        self.heads = {}
        self.proofs = {}
        self.denied = set()
        self.consulted = []
        self.confirm_allowed = True

    def get_registration(self, ref):
        self.consulted.append(ref)
        return self.facts.get(ref)

    def current_registration(self, fact_id):
        return self.heads.get(fact_id)

    def prove(self, fact, path, evidence_ref):
        return self.proofs.get((fact, path, evidence_ref))

    def admit(self, operation, registrations):
        caller = reqctx.current()
        if caller != (WRITER if operation == "confirm_registration" else READER):
            raise PermissionError("request scope rejected")
        if self.denied.intersection(registrations):
            raise PermissionError("fixed source closure denied")
        return lambda: (
            reqctx.current() == caller and self.confirm_allowed and not self.denied.intersection(registrations)
        )

    def add(self, *, name="合成主体甲", identifier=TEST_USCC, previous=None, version=None, **changes):
        raw_hash = "sha256:" + ("a" if name == "合成主体甲" else "b") * 64
        source = RegistrationSource("synthetic-registry", "record:" + ids.new("ogr"), ids.new("snp"), raw_hash)
        version = version or (previous.ref.version + 1 if previous else 1)
        if previous:
            source = replace(source, source_id=previous.source.source_id, source_key=previous.source.source_key)
        ref = RegistrationRef(previous.ref.fact_id if previous else ids.new("ogr"), version)
        path = "/uscc" if identifier and identifier.namespace == "cn-uscc" else "/registry_subject_id"
        identifier_field = EvidenceField(path, identifier.value, ids.new("ev")) if identifier else None
        reg = Registration(ref, ids.new("exr"), source, "public_registry", "organization",
                           EvidenceField("/name/value", name, ids.new("ev")), identifier, identifier_field)
        reg = replace(reg, **changes)
        self.facts[ref] = reg
        if ref.version > getattr(self.heads.get(ref.fact_id), "version", 0):
            self.heads[ref.fact_id] = ref
        for field in (reg.name, reg.identifier_field):
            if field is not None:
                self.proofs[ref, field.path, field.evidence_ref] = RegistrationEvidence(
                    ref, reg.revision_ref, reg.source, field
                )
        return reg


@pytest.fixture
def authority(pg_factory):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    database = db.Database(dsns["app"])
    owners = Owners()

    def bind():
        return public_organizations(database, registrations=owners, evidence=owners, admit=owners.admit)

    try:
        yield database, owners, bind
    finally:
        database.close()


def ingest(bind, reg):
    with reqctx.use(WRITER):
        return bind().confirm_registration(reg.ref)


def counts(database):
    with reqctx.use(WRITER), database.tx() as conn:
        return conn.execute(text("""
            SELECT (SELECT count(*) FROM ent_organizations), (SELECT count(*) FROM ent_organization_versions)
        """)).one()


def test_fixed_version_current_and_known_at_survive_reopen_and_replay(authority):
    database, owners, bind = authority
    a = owners.add()
    v1 = ingest(bind, a).organization
    b = owners.add(previous=a, name="合成主体甲更名")
    with reqctx.use(READER):
        assert bind().get_entity(v1.org_id) is None
        assert bind().get_entity(v1.org_id, version=1).organization == v1
    v2 = ingest(bind, b).organization
    a_again = owners.add(previous=b, name=a.name.value)
    v3 = ingest(bind, a_again).organization
    assert v1.org_id == v2.org_id == v3.org_id
    assert (v1.version, v2.version, v3.version) == (1, 2, 3)
    assert v1.recorded_at < v2.recorded_at < v3.recorded_at
    assert v1.source.source_key == v2.source.source_key == v3.source.source_key
    assert v1.source.raw_sha256 == v3.source.raw_sha256 != v2.source.raw_sha256
    assert v1.source.snapshot_ref != v3.source.snapshot_ref
    assert ingest(bind, a).organization == v1
    assert ingest(bind, a_again).organization == v3
    assert counts(database) == (1, 3)

    database.close()
    with reqctx.use(READER):
        reader = bind()
        assert reader.get_entity(v1.org_id).organization == v3
        assert reader.get_entity(v1.org_id, version=2).organization == v2
        assert reader.get_entity(v1.org_id, version=1, require_current=True) is None
        assert reader.get_entity(v1.org_id, version=3, require_current=True).organization == v3
        historical = reader.get_entity(v1.org_id, recorded_cutoff=v1.recorded_at)
        assert historical.organization == v1 and historical.recorded_cutoff == v1.recorded_at
        assert reader.get_entity(v1.org_id, version=2, recorded_cutoff=v1.recorded_at) is None
        assert reader.get_entity(v1.org_id, recorded_cutoff=v1.recorded_at - timedelta(microseconds=1)) is None
        assert reader.resolve_entity(identifier=TEST_USCC, name=a.name.value).organization == v3
        assert reader.resolve_entity(name=b.name.value).status == "unknown"


def test_names_lot_only_branch_and_identity_conflicts_never_select_an_org(authority):
    database, owners, bind = authority
    first = owners.add(identifier=OrganizationIdentifier(NAMESPACE, "X1"))
    other = owners.add(identifier=OrganizationIdentifier(NAMESPACE, "X2"))
    branch = owners.add(identifier=OrganizationIdentifier(NAMESPACE, "X3"), subject_kind="branch")
    weak = owners.add(identifier=None)
    synthetic_fixture = owners.add(scope="fixture")
    conflicting_stream = owners.add(identifier=first.identifier)
    independent_namespace = owners.add(identifier=OrganizationIdentifier("other-synthetic-authority", "X1"),
                                       name="另一机构")

    first_result = ingest(bind, first)
    other_result = ingest(bind, other)
    assert first_result.organization.org_id != other_result.organization.org_id
    assert ingest(bind, independent_namespace).status == "confirmed"
    for reg in (branch, weak):
        decision = ingest(bind, reg)
        assert decision.status == "candidate" and decision.organization is None
    assert ingest(bind, synthetic_fixture).status == "unknown"

    with reqctx.use(READER):
        reader = bind()
        assert reader.resolve_entity(name=first.name.value).status == "ambiguous"
        assert reader.resolve_entity(name=independent_namespace.name.value).status == "candidate"
        assert reader.resolve_entity(name="仅有标包文字").status == "unknown"
        assert reader.resolve_entity().status == "unknown"
        assert reader.resolve_entity(identifier=branch.identifier).status == "unknown"
        mismatch = reader.resolve_entity(identifier=first.identifier, name="合成主体甲分公司")
        assert mismatch.status == "candidate" and mismatch.organization is None
        assert reader.resolve_entity(identifier=first.identifier).organization == first_result.organization

    assert ingest(bind, conflicting_stream).status == "candidate"
    assert ingest(bind, conflicting_stream).status == "candidate"
    with reqctx.use(READER):
        reader = bind()
        org = first_result.organization
        assert reader.resolve_entity(identifier=first.identifier).status == "unknown"
        assert reader.get_entity(org.org_id, version=org.version, require_current=True) is None
        assert reader.get_entity(org.org_id, recorded_cutoff=datetime.now(org.recorded_at.tzinfo)) is None
        assert reader.get_entity(org.org_id, recorded_cutoff=org.recorded_at).organization == org

    changed_key = owners.add(previous=first, identifier=OrganizationIdentifier(NAMESPACE, "X4"))
    assert ingest(bind, changed_key).status == "candidate"
    with reqctx.use(READER):
        reader = bind()
        org_id = first_result.organization.org_id
        assert reader.resolve_entity(identifier=first.identifier).status == "unknown"
        assert reader.get_entity(org_id) is None
        assert reader.get_entity(org_id, version=1).organization == first_result.organization
    assert counts(database) == (3, 3)
    with reqctx.use(WRITER), database.tx() as conn:
        assert conn.execute(text("SELECT count(*) FROM ent_organization_conflicts")).scalar_one() == 2


@pytest.mark.parametrize("corruption", ["wrong_fact", "wrong_version", "wrong_revision", "wrong_snapshot",
                                       "wrong_source", "wrong_key", "wrong_hash", "wrong_field", "wrong_value",
                                       "missing_proof", "wrong_checksum"])
def test_rejects_unbound_or_mismatched_owner_proof(authority, corruption):
    database, owners, bind = authority
    reg = owners.add()
    field = reg.identifier_field
    key = reg.ref, field.path, field.evidence_ref
    proof = owners.proofs[key]
    if corruption == "wrong_fact":
        owners.facts[reg.ref] = replace(reg, ref=replace(reg.ref, fact_id=ids.new("ogr")))
    elif corruption == "wrong_version":
        owners.proofs[key] = replace(proof, fact=replace(reg.ref, version=2))
    elif corruption == "wrong_revision":
        owners.proofs[key] = replace(proof, revision_ref=ids.new("exr"))
    elif corruption == "wrong_snapshot":
        owners.proofs[key] = replace(proof, source=replace(reg.source, snapshot_ref=ids.new("snp")))
    elif corruption == "wrong_source":
        owners.proofs[key] = replace(proof, source=replace(reg.source, source_id="another-registry"))
    elif corruption == "wrong_key":
        owners.proofs[key] = replace(proof, source=replace(reg.source, source_key="another-record"))
    elif corruption == "wrong_hash":
        owners.proofs[key] = replace(proof, source=replace(reg.source, raw_sha256="sha256:" + "b" * 64))
    elif corruption == "wrong_field":
        owners.proofs[key] = replace(proof, field=replace(field, path="/subject/uscc"))
    elif corruption == "wrong_value":
        owners.proofs[key] = replace(proof, field=replace(field, value="unrelated"))
    elif corruption == "missing_proof":
        del owners.proofs[key]
    else:
        owners.facts[reg.ref] = replace(reg, identifier=replace(TEST_USCC, value=TEST_USCC.value[:-1] + "0"))
    with pytest.raises(OrganizationProofError):
        ingest(bind, reg)
    assert counts(database) == (0, 0)


def test_request_scope_current_rights_and_confirmation_fail_closed(authority):
    database, owners, bind = authority
    reg = owners.add()
    with reqctx.use(READER), pytest.raises(PermissionError):
        bind().confirm_registration(reg.ref)
    with reqctx.use(WRITER), pytest.raises(PermissionError):
        bind().resolve_entity()
    assert not owners.consulted

    owners.denied.add(reg.ref)
    with pytest.raises(PermissionError):
        ingest(bind, reg)
    assert not owners.consulted and counts(database) == (0, 0)

    owners.denied.clear()
    peek = owners.current_registration

    def revoke_in_transaction(fact_id):
        result = peek(fact_id)
        owners.confirm_allowed = False
        return result

    owners.current_registration = revoke_in_transaction
    with pytest.raises(PermissionError):
        ingest(bind, reg)
    assert counts(database) == (0, 0)
    owners.confirm_allowed = True
    owners.current_registration = peek
    version = ingest(bind, reg).organization

    with reqctx.use(READER):
        reader = bind()
        owners.denied.add(reg.ref)
        with pytest.raises(PermissionError):
            reader.get_entity(version.org_id, version=1)
        with pytest.raises(PermissionError):
            reader.get_entity(version.org_id, recorded_cutoff=version.recorded_at)
        with pytest.raises(PermissionError):
            reader.resolve_entity(name=reg.name.value)
        owners.denied.clear()
        owners.current_registration = revoke_in_transaction
        with pytest.raises(PermissionError):
            reader.resolve_entity(identifier=reg.identifier)
    assert counts(database) == (1, 1)


def test_fixed_owner_projection_mutation_cannot_pass_replay_or_read(authority):
    database, owners, bind = authority
    reg = owners.add()
    stable = ingest(bind, reg).organization
    reg_changed = replace(reg, name=replace(reg.name, value="changed"))
    owners.facts[reg.ref] = reg_changed
    owners.proofs[reg.ref, reg.name.path, reg.name.evidence_ref] = RegistrationEvidence(
        reg.ref, reg.revision_ref, reg.source, reg_changed.name
    )
    with pytest.raises(OrganizationProofError):
        ingest(bind, reg)
    with reqctx.use(READER), pytest.raises(OrganizationProofError):
        bind().get_entity(stable.org_id, version=stable.version)
    assert counts(database) == (1, 1)


def test_late_fact_and_concurrent_retries_do_not_rewind_or_fork(authority):
    database, owners, bind = authority
    initial = owners.add()
    current = owners.add(previous=initial)
    assert ingest(bind, initial).status == "unknown"
    barrier = Barrier(4)

    def ingest_concurrently():
        barrier.wait(timeout=10)
        return ingest(bind, current)

    with ThreadPoolExecutor(max_workers=4) as pool:
        writes = [pool.submit(ingest_concurrently) for _ in range(4)]
        decisions = [write.result(timeout=10) for write in writes]
    assert decisions[0].status == "confirmed"
    assert all(decision == decisions[0] for decision in decisions)
    assert decisions[0].organization.version == 1 and decisions[0].organization.registration == current.ref
    assert ingest(bind, initial).status == "unknown"
    assert counts(database) == (1, 1)


def test_concurrent_distinct_registrations_cannot_claim_one_identifier(authority):
    database, owners, bind = authority
    first, second = owners.add(), owners.add()
    barrier = Barrier(2)

    def try_claim(reg):
        barrier.wait(timeout=10)
        return ingest(bind, reg)

    with ThreadPoolExecutor(max_workers=2) as pool:
        decisions = list(pool.map(try_claim, (first, second)))
    assert sorted(decision.status for decision in decisions) == ["candidate", "confirmed"]
    confirmed = next(decision.organization for decision in decisions if decision.status == "confirmed")
    with reqctx.use(READER):
        assert bind().resolve_entity(identifier=TEST_USCC).status == "unknown"
        assert bind().get_entity(confirmed.org_id, version=confirmed.version).organization == confirmed
    assert counts(database) == (1, 1)


def test_conflict_requires_write_admission_for_both_fixed_registrations(authority):
    database, owners, bind = authority
    first, contender = owners.add(), owners.add()
    confirmed = ingest(bind, first).organization
    owners.denied.add(first.ref)
    with pytest.raises(PermissionError):
        ingest(bind, contender)
    owners.denied.clear()
    with reqctx.use(READER):
        assert bind().get_entity(confirmed.org_id, require_current=True).organization == confirmed
    with reqctx.use(WRITER), database.tx() as conn:
        assert conn.execute(text("SELECT count(*) FROM ent_organization_conflicts")).scalar_one() == 0


@pytest.mark.parametrize("statement", ["UPDATE ent_organizations SET source_key = 'changed'",
                                     "DELETE FROM ent_organization_versions",
                                     "TRUNCATE ent_organizations CASCADE"])
def test_app_role_cannot_change_committed_identity_or_versions(authority, statement):
    database, owners, bind = authority
    reg = owners.add()
    assert ingest(bind, reg).status == "confirmed"
    with reqctx.use(WRITER), pytest.raises(DBAPIError), database.tx() as conn:
        conn.execute(text(statement))
    assert counts(database) == (1, 1)


def test_input_shapes_reject_version_coercion_and_unscoped_time(authority):
    _, _, bind = authority
    with reqctx.use(READER):
        reader = bind()
        for version in (True, 0, 1 << 63):
            with pytest.raises(ValueError):
                reader.get_entity(ids.new("org"), version=version)
        with pytest.raises(ValueError):
            reader.get_entity(ids.new("org"), recorded_cutoff=datetime(2026, 1, 1))
        with pytest.raises(ValueError):
            reader.resolve_entity(identifier=OrganizationIdentifier("cn-uscc", TEST_USCC.value[:-1] + "0"))
