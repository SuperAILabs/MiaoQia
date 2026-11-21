"""Trusted local composition for confirming organization registrations."""
from __future__ import annotations

from collections.abc import Callable, Collection
from typing import Protocol

from miaoqia.acquisition.api import SourceInput, identifier as check_source_id
from miaoqia.domain_intelligence.api.registrations import RegistrationFacts
from miaoqia.entity_graph import public_organizations
from miaoqia.entity_graph.api.organizations import (
    EvidenceField,
    OrganizationIdentifier,
    OrganizationOperation,
    OrganizationProofError,
    Organizations,
    Registration,
    RegistrationEvidence,
    RegistrationRef,
    RegistrationSource,
)
from miaoqia.evidence import source_requirements
from miaoqia.evidence.api import Closure, Extractions
from miaoqia.evidence.api.document_proofs import DocumentProofs, DocumentSource
from miaoqia.kernel import reqctx
from miaoqia.kernel.db import Database
from miaoqia.kernel.jsoncodec import loads
from miaoqia.kernel.objectstore import ObjectStore

_ORGANIZATION_TYPES = frozenset({
    "limited_liability", "joint_stock", "state_owned", "collective", "partnership",
    "sole_proprietorship", "foreign_invested", "public_institution", "social_organization",
})


class OrganizationAccess(Protocol):
    def __call__(self, caller: reqctx.Context, operation: OrganizationOperation,
                 groups: tuple[SourceInput, ...]) -> Callable[[], bool]:
        """Admit the caller and operation. For groups, require discovery, display and full proof reads.

        Reconfirm the same caller, operation and groups against current authority on each use.
        """
        ...


def parse_registry_sources(raw: str) -> tuple[str, ...]:
    value = loads(raw)
    if not isinstance(value, list) or not value:
        raise ValueError("nonempty registry source allowlist required")
    sources = tuple(check_source_id(source) for source in value)
    if len(set(sources)) != len(sources):
        raise ValueError("duplicate registry source")
    return sources


def bind_organizations(entities: Database, domain: Database, evidence: Database, store: ObjectStore, *,
                       extractions: Extractions, approved_sources: Collection[str],
                       authorize: OrganizationAccess) -> Organizations:
    """Bind trusted databases, object storage, registry allowlist and current policy explicitly."""
    ports = _owner_ports(domain, evidence, store, extractions, approved_sources, authorize)
    return public_organizations(entities, registrations=ports, evidence=ports, admit=ports.admit)


def bind_organization_search(entities, domain, evidence, store, *, extractions, approved_sources, authorize, policy):
    from miaoqia.entity_graph.api.search_projection import owner

    ports = _owner_ports(domain, evidence, store, extractions, approved_sources, authorize)
    authority = public_organizations(entities, registrations=ports, evidence=ports, admit=ports.admit)
    return owner(authority, policy, admit_public=ports.admit_discovery)


def _owner_ports(domain, evidence, store, extractions, approved_sources, authorize):
    if isinstance(approved_sources, (str, bytes)) or not approved_sources or not callable(authorize):
        raise ValueError("explicit registry source allowlist and authorization required")
    sources = frozenset(check_source_id(value) for value in approved_sources)
    return _RegistrationPorts(RegistrationFacts(domain, extractions), DocumentProofs(evidence, store),
                              source_requirements(evidence), sources, authorize)


def _fields(fact):
    name = fact["name"]
    name_field = EvidenceField("/name/value", name["value"], name["prov"]["evidence_ref"])
    if "uscc" not in fact:
        return (name_field,)
    return name_field, EvidenceField("/uscc", fact["uscc"], fact["uscc_prov"]["evidence_ref"])


def _subject_kind(fact):
    kind = fact.get("enterprise_type")
    if kind == "branch" or fact.get("parent") is not None:
        return "branch"
    return "organization" if kind in _ORGANIZATION_TYPES else "unknown"


class _RegistrationPorts:
    def __init__(self, facts, documents, provenance, approved_sources, authorize):
        self.facts = facts
        self.documents = documents
        self.provenance = provenance
        self.approved_sources = approved_sources
        self.authorize = authorize

    def _eligible(self, ref):
        fixed = self.facts.get(ref.fact_id, ref.version)
        if fixed is None or fixed.fact.get("record_scope") is not None or fixed.fact["classification"] != "public":
            return None
        if fixed.source_id not in self.approved_sources:
            raise PermissionError("registry source is not approved")
        return fixed

    @staticmethod
    def _source(fixed, actual: DocumentSource):
        if (actual.source_id, actual.source_key, actual.snapshot_ref) != (
            fixed.source_id, fixed.source_key, fixed.fact["snapshot_ref"]
        ):
            raise OrganizationProofError("registry source does not match the document snapshot")
        return RegistrationSource(actual.source_id, actual.source_key, actual.snapshot_ref, actual.raw_sha256)

    def admit(self, operation, registrations):
        closures = []
        for ref in registrations:
            fixed = self._eligible(ref)
            if fixed is None:
                raise PermissionError("eligible fixed registration unavailable")
            closures.append(self._closure(fixed))
        groups = self.provenance.resolve(Closure.merge(*closures)) if closures else ()
        return self._receipt(operation, groups)

    def _closure(self, fixed):
        fact = fixed.fact
        snapshot = fact["snapshot_ref"]
        refs = tuple(field.evidence_ref for field in _fields(fact))
        return Closure.merge(fixed.closure, self.provenance.revision(fact["revision_ref"], snapshot),
                             self.documents.requirements(snapshot, refs))

    def admit_discovery(self, ref, source_id, *, deadline):
        fixed = self._eligible(ref)
        if fixed is None or fixed.source_id != source_id:
            return None
        closure = self._closure(fixed)
        expected = SourceInput(source_id, (f"public:{source_id}",))
        if closure.references or not closure.dependencies or closure.sources != (expected,):
            return None
        groups = self.provenance.resolve(closure, deadline=deadline)
        if tuple(groups) != (expected,):
            return None
        try:
            return self._receipt("get_entity", groups)
        except PermissionError:
            return None

    def _receipt(self, operation, groups):
        caller = reqctx.current()
        caller.validate()
        receipt = self.authorize(caller, operation, groups)

        def confirm():
            return reqctx.get() == caller and callable(receipt) and receipt() is True

        return confirm

    def current_registration(self, fact_id):
        version = self.facts.current_version(fact_id)
        return RegistrationRef(fact_id, version) if version is not None else None

    def get_registration(self, ref):
        fixed = self._eligible(ref)
        if fixed is None:
            return None
        fact = fixed.fact
        name, *identifiers = _fields(fact)
        field = identifiers[0] if identifiers else None
        identifier = OrganizationIdentifier("cn-uscc", field.value) if field else None
        subject = fact["subject"]
        if (subject["name_raw"] != name.value
                or ("uscc" in subject and (identifier is None or subject["uscc"] != identifier.value))):
            raise OrganizationProofError("registered name or USCC differs from the subject")
        source = self._source(fixed, self.documents.source(fact["snapshot_ref"]))
        return Registration(ref, fact["revision_ref"], source, "public_registry", _subject_kind(fact),
                            name, identifier, field)

    def prove(self, fact, path, evidence_ref):
        fixed = self._eligible(fact)
        if fixed is None:
            return None
        field = next((item for item in _fields(fixed.fact)
                      if (item.path, item.evidence_ref) == (path, evidence_ref)), None)
        if field is None:
            return None
        source = self._source(fixed, self.documents.prove(fixed.fact["snapshot_ref"], evidence_ref, field.value))
        return RegistrationEvidence(fact, fixed.fact["revision_ref"], source, field)
