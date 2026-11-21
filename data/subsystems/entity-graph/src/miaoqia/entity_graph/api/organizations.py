"""Public organization authority. Owner ports are installed by the trusted runtime."""
from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Callable, Literal, Protocol


@dataclass(frozen=True)
class RegistrationRef:
    fact_id: str
    version: int


@dataclass(frozen=True)
class OrganizationIdentifier:
    """Use cn-uscc for a Chinese USCC, or a deployment-approved authority namespace."""

    namespace: str
    value: str


@dataclass(frozen=True)
class RegistrationSource:
    source_id: str
    source_key: str
    snapshot_ref: str
    raw_sha256: str


@dataclass(frozen=True)
class EvidenceField:
    path: str
    value: str
    evidence_ref: str


@dataclass(frozen=True)
class Registration:
    """Owner projection of one fixed INF-ORG organization.registration fact.

    Scope and subject kind come from verified owner data. A public_registry
    projection requires an approved authoritative registry source, a public
    fact, and an identifier issued to the registered subject in that namespace.
    """

    ref: RegistrationRef
    revision_ref: str
    source: RegistrationSource
    scope: Literal["public_registry", "fixture", "ineligible"]
    subject_kind: Literal["organization", "branch", "unknown"]
    name: EvidenceField
    identifier: OrganizationIdentifier | None = None
    identifier_field: EvidenceField | None = None


@dataclass(frozen=True)
class RegistrationEvidence:
    """Document owner attestation bound to the actual fixed fact field."""

    fact: RegistrationRef
    revision_ref: str
    source: RegistrationSource
    field: EvidenceField


@dataclass(frozen=True)
class OrganizationVersion:
    org_id: str
    version: int
    recorded_at: datetime
    identifier: OrganizationIdentifier
    name: str
    registration: RegistrationRef
    revision_ref: str
    source: RegistrationSource
    fields: tuple[EvidenceField, EvidenceField]


@dataclass(frozen=True)
class OrganizationRead:
    organization: OrganizationVersion
    recorded_cutoff: datetime | None


@dataclass(frozen=True)
class OrganizationResolution:
    status: Literal["confirmed", "candidate", "ambiguous", "unknown"]
    organization: OrganizationVersion | None = None


class OrganizationProofError(ValueError):
    """Owner metadata, a fixed field, or its evidence failed verification."""


OrganizationOperation = Literal["confirm_registration", "get_entity", "resolve_entity"]


class RegistrationOwner(Protocol):
    def get_registration(self, ref: RegistrationRef) -> Registration | None: ...

    def current_registration(self, fact_id: str) -> RegistrationRef | None:
        """Current eligible owner head. Return None for a missing or withdrawn head.

        Unavailable or unverifiable authority raises an error.
        """
        ...


class RegistrationEvidenceOwner(Protocol):
    def prove(self, fact: RegistrationRef, path: str, evidence_ref: str) -> RegistrationEvidence | None:
        """Verify the owner field/locator, document snapshot and original byte hash."""
        ...


class OrganizationAdmission(Protocol):
    def __call__(self, operation: OrganizationOperation,
                 registrations: tuple[RegistrationRef, ...]) -> Callable[[], bool]:
        """Authorize reqctx caller and operation plus full fixed fact/evidence closure.

        An empty tuple admits the request itself. For nonempty inputs, authorize
        discovery and display of the projected fields and evidence handles too.
        Return a receipt confirmation for the same caller, operation and closure.
        """
        ...


class OrganizationReader(Protocol):
    def get_entity(self, org_id: str, *, version: int | None = None,
                   recorded_cutoff: datetime | None = None,
                   require_current: bool = False) -> OrganizationRead | None:
        """Read the latest eligible, explicitly pinned, or then-known version.

        A cutoff bounds pinned reads as well. Search uses require_current to
        suppress superseded pins. Every selection uses current authorization.
        """
        ...

    def resolve_entity(self, *, identifier: OrganizationIdentifier | None = None,
                       name: str | None = None) -> OrganizationResolution:
        """Resolve a current registry identity. A mention's identity needs its own proof.

        A name is an exact check or a weak signal. Only confirmed results include
        an organization version.
        """
        ...


class Organizations(OrganizationReader, Protocol):
    def confirm_registration(self, ref: RegistrationRef) -> OrganizationResolution: ...
