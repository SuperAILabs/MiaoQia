"""Proof-gated organization writes, current resolution and fixed-version reads."""
from __future__ import annotations

from datetime import datetime

from miaoqia.kernel import outbox, reqctx
from miaoqia.kernel.db import Database

from ..api import NAME
from ..api.organizations import (
    OrganizationAdmission,
    OrganizationIdentifier,
    OrganizationProofError,
    OrganizationRead,
    OrganizationResolution,
    OrganizationVersion,
    Registration,
    RegistrationEvidence,
    RegistrationEvidenceOwner,
    RegistrationOwner,
    RegistrationRef,
)
from ..domain.organizations import check_id, check_identifier, check_name, check_ref, check_registration, check_version
from ..infrastructure import organizations as store

_events = outbox.Writer(NAME, "entity")


def _changed(conn, organization, fields=()):
    data = {"entity_id": organization.org_id, "entity_version": organization.version,
            "rights_tags": [f"public:{organization.source.source_id}"]}
    if fields:
        data["changed_fields"] = list(fields)
        kind = "entity.entity.updated.v1"
    else:
        data["entity_type"] = "organization"
        kind = "entity.entity.created.v1"
    _events.append(conn, outbox.Event(kind, organization.org_id, data))


def _confirm(*receipts):
    for receipt in receipts:
        if not callable(receipt) or receipt() is not True:
            raise PermissionError("organization authorization denied or expired")


def _assert_same(reg: Registration, version: OrganizationVersion) -> None:
    if (reg.identifier, reg.revision_ref, reg.source, (reg.name, reg.identifier_field)) != (
        version.identifier, version.revision_ref, version.source, version.fields
    ):
        raise OrganizationProofError("fixed registration differs from the committed version")


class PublicOrganizations:
    def __init__(self, database: Database, *, registrations: RegistrationOwner,
                 evidence: RegistrationEvidenceOwner, admit: OrganizationAdmission):
        self.database = database
        self.registrations = registrations
        self.evidence = evidence
        self.admit = admit

    def _registration(self, ref: RegistrationRef) -> Registration | None:
        reg = self.registrations.get_registration(ref)
        if reg is None:
            return None
        if not isinstance(reg, Registration) or reg.ref != ref:
            raise OrganizationProofError("registration reference mismatch")
        if reg.scope not in ("public_registry", "fixture", "ineligible"):
            raise OrganizationProofError("unsupported registration scope")
        if reg.subject_kind not in ("organization", "branch", "unknown"):
            raise OrganizationProofError("unsupported registration subject kind")
        return reg

    def _prove(self, reg: Registration, ref: RegistrationRef) -> None:
        check_registration(reg, ref)
        for field in (reg.name, reg.identifier_field):
            actual = self.evidence.prove(ref, field.path, field.evidence_ref)
            if actual != RegistrationEvidence(ref, reg.revision_ref, reg.source, field):
                raise OrganizationProofError("field evidence differs from the fixed registration")

    def _check_read(self, version: OrganizationVersion, *, current: bool) -> bool:
        if current and self.registrations.current_registration(version.registration.fact_id) != version.registration:
            return False
        reg = self._registration(version.registration)
        if (reg is None or reg.scope != "public_registry" or reg.subject_kind != "organization"
                or reg.identifier is None):
            raise OrganizationProofError("confirmed registration unavailable")
        self._prove(reg, version.registration)
        _assert_same(reg, version)
        return True

    def confirm_registration(self, ref: RegistrationRef) -> OrganizationResolution:
        check_ref(ref)
        ctx = reqctx.current()
        if ctx.tenant or ctx.cell or ctx.principal.kind != "service" or not ctx.principal.id.startswith("svc:"):
            raise PermissionError("public organization writer requires a public service principal")
        admission = self.admit("confirm_registration", ())
        _confirm(admission)
        receipt = self.admit("confirm_registration", (ref,))
        _confirm(receipt)
        reg = self._registration(ref)
        result = OrganizationResolution("unknown")
        if reg is not None and reg.scope == "public_registry":
            if reg.subject_kind != "organization" or reg.identifier is None:
                result = OrganizationResolution("candidate")
            else:
                self._prove(reg, ref)
                with self.database.tx() as conn:
                    result = self._record(conn, reg)
                    _confirm(admission, receipt)
                return result
        _confirm(admission, receipt)
        return result

    def _record(self, conn, reg: Registration) -> OrganizationResolution:
        matches = store.lock_identity(conn, reg)
        head = None
        owner = next((row for row in matches if row.fact_id == reg.ref.fact_id), None)
        if owner is not None:
            existing = store.registered(conn, owner.org_id, reg.ref.version)
            if existing is not None:
                _assert_same(reg, existing)
                return OrganizationResolution("confirmed", existing)

        if matches:
            if len(matches) != 1 or owner is None or (
                owner.identifier_namespace, owner.identifier_value, owner.source_id, owner.source_key
            ) != (reg.identifier.namespace, reg.identifier.value, reg.source.source_id, reg.source.source_key):
                if self.registrations.current_registration(reg.ref.fact_id) == reg.ref:
                    org_ids = tuple(row.org_id for row in matches)
                    affected = tuple(store.get(conn, org_id) for org_id in org_ids)
                    if any(version is None for version in affected):
                        raise OrganizationProofError("organization has no committed version")
                    refs = tuple(dict.fromkeys((reg.ref, *(version.registration for version in affected))))
                    receipt = self.admit("confirm_registration", refs)
                    _confirm(receipt)
                    newly_disputed = tuple(version for version in affected
                                           if store.get(conn, version.org_id, undisputed=True) is not None)
                    store.dispute(conn, reg.ref, org_ids)
                    for version in newly_disputed:
                        _changed(conn, version, ("/registration_status",))
                    _confirm(receipt)
                return OrganizationResolution("candidate")

            head = store.get(conn, owner.org_id)
            if head is None:
                raise OrganizationProofError("organization has no committed version")
            if head.registration.version >= reg.ref.version:
                return OrganizationResolution("unknown")
            if store.get(conn, owner.org_id, undisputed=True) is None:
                return OrganizationResolution("candidate")

        if self.registrations.current_registration(reg.ref.fact_id) != reg.ref:
            return OrganizationResolution("unknown")
        organization = store.append(conn, reg, head)
        _changed(conn, organization, ("/registration", "/name") if head else ())
        return OrganizationResolution("confirmed", organization)

    def get_entity(self, org_id: str, *, version: int | None = None,
                   recorded_cutoff: datetime | None = None, require_current: bool = False) -> OrganizationRead | None:
        check_id(org_id, "org")
        if version is not None:
            check_version(version)
        if recorded_cutoff is not None and (not isinstance(recorded_cutoff, datetime)
                                            or recorded_cutoff.utcoffset() is None):
            raise ValueError("recorded cutoff must be timezone-aware")
        if type(require_current) is not bool or (require_current and recorded_cutoff is not None):
            raise ValueError("current-only and historical selection cannot be combined")

        reqctx.current()
        admission = self.admit("get_entity", ())
        _confirm(admission)
        current = require_current or (version is None and recorded_cutoff is None)
        with self.database.tx() as conn:
            selected = store.get(conn, org_id, None if require_current else version, recorded_cutoff,
                                 undisputed=require_current or version is None)
        if selected is None or (require_current and version is not None and selected.version != version):
            _confirm(admission)
            return None
        receipt = self.admit("get_entity", (selected.registration,))
        _confirm(receipt)
        visible = self._check_read(selected, current=current)
        _confirm(admission, receipt)
        return OrganizationRead(selected, recorded_cutoff) if visible else None

    def resolve_entity(self, *, identifier: OrganizationIdentifier | None = None,
                       name: str | None = None) -> OrganizationResolution:
        if identifier is not None:
            check_identifier(identifier)
        if name is not None:
            check_name(name)

        reqctx.current()
        admission = self.admit("resolve_entity", ())
        _confirm(admission)
        if identifier is not None or name is not None:
            with self.database.tx() as conn:
                if identifier is None:
                    selected = store.by_name(conn, name)
                else:
                    exact = store.by_identifier(conn, identifier)
                    selected = (exact,) if exact is not None else ()
        else:
            selected = ()
        if not selected:
            _confirm(admission)
            return OrganizationResolution("unknown")

        receipt = self.admit("resolve_entity", tuple(row.registration for row in selected))
        _confirm(receipt)
        visible = tuple(row for row in selected if self._check_read(row, current=True))
        _confirm(admission, receipt)
        if not visible:
            return OrganizationResolution("unknown")
        if identifier is None:
            return OrganizationResolution("ambiguous" if len(visible) > 1 else "candidate")
        if name is not None and name != visible[0].name:
            return OrganizationResolution("candidate")
        return OrganizationResolution("confirmed", visible[0])
