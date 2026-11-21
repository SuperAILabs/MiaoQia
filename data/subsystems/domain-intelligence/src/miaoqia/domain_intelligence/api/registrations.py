"""Trusted local reads of fixed organization registration facts and their lineage."""
from __future__ import annotations

from dataclasses import dataclass

from sqlalchemy import text

from miaoqia.evidence.api import Closure, Extractions, ProvenanceIntegrityError
from miaoqia.kernel.db import Database
from miaoqia.sdk.derivation import fingerprint

from ..application import requirements
from ..application.facts import fact_version
from ..application.source import validate_source


@dataclass(frozen=True)
class FixedRegistration:
    fact: dict
    source_id: str
    source_key: str
    closure: Closure


class RegistrationFacts:
    """Owner port for trusted runtime composition. Source admission is the caller's responsibility."""

    def __init__(self, database: Database, extractions: Extractions):
        self._database = database
        self._extractions = extractions

    def _read(self, conn, fact_id):
        conn.execute(text("SET TRANSACTION READ ONLY"))
        return conn.execute(text("""
            SELECT info_domain, source_id, source_key, ordinal, current_version
            FROM dom_facts WHERE id = :id AND info_domain = 'INF-ORG'
                AND object_type = 'organization.registration'
        """), {"id": fact_id}).one_or_none()

    def current_version(self, fact_id: str) -> int | None:
        with self._database.tx() as conn:
            row = self._read(conn, fact_id)
            return row.current_version if row is not None else None

    def get(self, fact_id: str, version: int) -> FixedRegistration | None:
        if type(version) is not int or version < 1:
            raise ValueError("fixed positive version required")
        with self._database.tx() as conn:
            row = self._read(conn, fact_id)
            if row is None:
                return None
            fact = fact_version(conn, fact_id, version)
            if fact is None:
                return None
            if (fact.get("id"), fact.get("version"), fact.get("info_domain")) != (fact_id, version, row.info_domain):
                raise ProvenanceIntegrityError("registration identity mismatch")
            closure = requirements.read(conn, fact)

        revision = self._extractions.revision(fact["revision_ref"])
        try:
            validate_source(fact, row, revision)
        except ValueError as exc:
            raise ProvenanceIntegrityError("registration differs from its fixed extraction") from exc
        if closure != requirements.base(revision):
            raise ProvenanceIntegrityError("registration is missing its direct extraction proof")
        return FixedRegistration(fact, row.source_id, row.source_key,
                                 closure.with_dependency("fact", f"{fact_id}:{version}", fingerprint(fact)))
