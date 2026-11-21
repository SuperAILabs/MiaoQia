"""Validation for a registration identity and the fields used to confirm it."""
from __future__ import annotations

import re

from ..api.organizations import (
    EvidenceField,
    OrganizationIdentifier,
    OrganizationProofError,
    Registration,
    RegistrationRef,
    RegistrationSource,
)

_ULID = r"[0-9A-HJKMNP-TV-Z]{26}"
_USCC_ALPHABET = "0123456789ABCDEFGHJKLMNPQRTUWXY"
_USCC_WEIGHTS = (1, 3, 9, 27, 19, 26, 16, 17, 20, 29, 25, 13, 8, 24, 10, 30, 28)
_USCC = re.compile(r"[0-9A-HJ-NPQRTUWXY]{2}[0-9]{6}[0-9A-HJ-NPQRTUWXY]{10}")


def check_id(value: str, prefix: str) -> None:
    if not isinstance(value, str) or not re.fullmatch(prefix + "_" + _ULID, value):
        raise ValueError(f"invalid {prefix} identifier")


def check_version(version: int) -> None:
    if type(version) is not int or not 0 < version < 1 << 63:
        raise ValueError("invalid version")


def check_ref(ref: RegistrationRef) -> None:
    if not isinstance(ref, RegistrationRef):
        raise ValueError("fixed registration reference required")
    check_id(ref.fact_id, "ogr")
    check_version(ref.version)


def check_name(name: str) -> None:
    if not isinstance(name, str) or len(name) > 512 or not name.strip() or "\x00" in name:
        raise ValueError("invalid organization name")


def check_identifier(identifier: OrganizationIdentifier) -> None:
    if (not isinstance(identifier, OrganizationIdentifier)
            or not isinstance(identifier.namespace, str)
            or not re.fullmatch(r"[a-z][a-z0-9_-]{0,62}", identifier.namespace)
            or not isinstance(identifier.value, str)
            or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}", identifier.value)):
        raise ValueError("invalid organization identifier")
    if identifier.namespace == "cn-uscc":
        code = identifier.value
        if not _USCC.fullmatch(code):
            raise ValueError("invalid USCC format")
        total = sum(_USCC_ALPHABET.index(char) * weight for char, weight in zip(code[:-1], _USCC_WEIGHTS))
        if _USCC_ALPHABET[-total % 31] != code[-1]:
            raise ValueError("invalid USCC checksum")


def _check_field(field: EvidenceField) -> None:
    if (not isinstance(field, EvidenceField) or not isinstance(field.path, str)
            or len(field.path) > 256
            or not re.fullmatch(r"(?:/(?:[^~/\x00]|~[01])+)+", field.path)):
        raise ValueError("invalid field path")
    check_id(field.evidence_ref, "ev")


def check_registration(reg: Registration, ref: RegistrationRef) -> None:
    if not isinstance(reg, Registration) or reg.ref != ref:
        raise OrganizationProofError("registration reference mismatch")
    try:
        check_ref(reg.ref)
        check_id(reg.revision_ref, "exr")
        s = reg.source
        if (not isinstance(s, RegistrationSource)
                or not isinstance(s.source_id, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_.-]{0,127}", s.source_id)
                or not isinstance(s.source_key, str) or not s.source_key.strip()
                or len(s.source_key) > 512 or "\x00" in s.source_key
                or not isinstance(s.raw_sha256, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", s.raw_sha256)):
            raise ValueError("invalid registration source")
        check_id(s.snapshot_ref, "snp")
        _check_field(reg.name)
        check_name(reg.name.value)
        if reg.name.path != "/name/value":
            raise ValueError("invalid registration name path")
        check_identifier(reg.identifier)
        _check_field(reg.identifier_field)
        field = reg.identifier_field
        if (field.value != reg.identifier.value or field.path == reg.name.path
                or field.evidence_ref == reg.name.evidence_ref
                or (reg.identifier.namespace == "cn-uscc" and field.path != "/uscc")):
            raise ValueError("identifier proof does not match the registration field")
    except ValueError as exc:
        raise OrganizationProofError(str(exc)) from exc
