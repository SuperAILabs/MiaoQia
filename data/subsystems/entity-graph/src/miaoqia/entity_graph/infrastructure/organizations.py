"""SQL persistence for immutable organization identities and versions in ent."""
from __future__ import annotations

from datetime import datetime

from sqlalchemy import Connection, text

from miaoqia.kernel import ids

from ..api.organizations import (
    EvidenceField,
    OrganizationIdentifier,
    OrganizationVersion,
    Registration,
    RegistrationRef,
    RegistrationSource,
)

_VERSION_FIELDS = """
o.*, v.version, v.fact_version, v.recorded_at, v.name, v.revision_ref,
       v.snapshot_ref, v.raw_sha256, v.name_evidence_ref, v.identifier_path, v.identifier_evidence_ref
"""
_SELECT = "SELECT " + _VERSION_FIELDS + """
FROM ent_organizations o JOIN ent_organization_versions v USING (org_id)
"""
_UNDISPUTED = """
    NOT EXISTS (SELECT 1 FROM ent_organization_conflicts c WHERE c.org_id = o.org_id)
"""


def _version(row) -> OrganizationVersion:
    return OrganizationVersion(
        org_id=row.org_id, version=row.version, recorded_at=row.recorded_at,
        identifier=OrganizationIdentifier(row.identifier_namespace, row.identifier_value), name=row.name,
        registration=RegistrationRef(row.fact_id, row.fact_version), revision_ref=row.revision_ref,
        source=RegistrationSource(row.source_id, row.source_key, row.snapshot_ref, row.raw_sha256),
        fields=(EvidenceField("/name/value", row.name, row.name_evidence_ref),
                EvidenceField(row.identifier_path, row.identifier_value, row.identifier_evidence_ref)),
    )


def get(conn: Connection, org_id: str, version: int | None = None,
        cutoff: datetime | None = None, *, undisputed: bool = False) -> OrganizationVersion | None:
    row = conn.execute(text(_SELECT + """
        WHERE o.org_id = :id
          AND (CAST(:version AS bigint) IS NULL OR v.version = :version)
          AND (CAST(:cutoff AS timestamptz) IS NULL OR v.recorded_at <= :cutoff)
          AND (NOT :undisputed OR NOT EXISTS (
              SELECT 1 FROM ent_organization_conflicts c WHERE c.org_id = o.org_id
                AND (CAST(:cutoff AS timestamptz) IS NULL OR c.recorded_at <= :cutoff)
          ))
        ORDER BY v.version DESC LIMIT 1
    """), {"id": org_id, "version": version, "cutoff": cutoff, "undisputed": undisputed}).one_or_none()
    return _version(row) if row else None


def by_identifier(conn: Connection, identifier: OrganizationIdentifier) -> OrganizationVersion | None:
    row = conn.execute(text(_SELECT + """
        WHERE o.identifier_namespace = :namespace AND o.identifier_value = :value AND """ + _UNDISPUTED + """
        ORDER BY v.version DESC LIMIT 1
    """), {"namespace": identifier.namespace, "value": identifier.value}).one_or_none()
    return _version(row) if row else None


def by_name(conn: Connection, name: str) -> tuple[OrganizationVersion, ...]:
    rows = conn.execute(text(_SELECT + """
        WHERE v.name = :name AND """ + _UNDISPUTED + """ AND NOT EXISTS (
            SELECT 1 FROM ent_organization_versions newer
            WHERE newer.org_id = v.org_id AND newer.version > v.version
        )
        ORDER BY o.org_id LIMIT 2
    """), {"name": name})
    return tuple(map(_version, rows))


def lock_identity(conn: Connection, reg: Registration):
    # Both sides of the one-to-one binding serialize before any existence check.
    identifier = reg.identifier
    keys = (f"ent:fact:{reg.ref.fact_id}", f"ent:identifier:{identifier.namespace}:{identifier.value}")
    for key in keys:
        conn.execute(text("SELECT pg_advisory_xact_lock(hashtextextended(:key, 0))"), {"key": key})
    return conn.execute(text("""
        SELECT org_id, fact_id, identifier_namespace, identifier_value, source_id, source_key
        FROM ent_organizations
        WHERE fact_id = :fact_id OR (identifier_namespace = :namespace AND identifier_value = :value)
        ORDER BY org_id
    """), {"fact_id": reg.ref.fact_id, "namespace": identifier.namespace, "value": identifier.value}).all()


def registered(conn: Connection, org_id: str, fact_version: int) -> OrganizationVersion | None:
    row = conn.execute(text(_SELECT + "WHERE o.org_id = :id AND v.fact_version = :version"),
                       {"id": org_id, "version": fact_version}).one_or_none()
    return _version(row) if row else None


def projection_heads(conn: Connection, org_ids: tuple[str, ...]):
    rows = conn.execute(text("SELECT " + _VERSION_FIELDS + ", (" + _UNDISPUTED + ") AS undisputed, " + """
        p.public_version, p.revision, p.digest
        FROM ent_organizations o JOIN LATERAL (
            SELECT * FROM ent_organization_versions WHERE org_id = o.org_id ORDER BY version DESC LIMIT 1
        ) v ON TRUE
        LEFT JOIN ent_search_projection p ON p.org_id = o.org_id
        WHERE o.org_id = ANY(:ids) ORDER BY o.org_id COLLATE "C"
    """), {"ids": list(org_ids)})
    return tuple((_version(row), row.undisputed, row if row.revision is not None else None) for row in rows)


def dispute(conn: Connection, ref: RegistrationRef, org_ids: tuple[str, ...]) -> None:
    conn.execute(text("""
        INSERT INTO ent_organization_conflicts (org_id, contender_fact_id, contender_fact_version)
        VALUES (:org_id, :fact_id, :version) ON CONFLICT DO NOTHING
    """), [{"org_id": org_id, "fact_id": ref.fact_id, "version": ref.version} for org_id in org_ids])


def append(conn: Connection, reg: Registration, head: OrganizationVersion | None) -> OrganizationVersion:
    source = reg.source
    if head is None:
        org_id = ids.new("org")
        conn.execute(text("""
            INSERT INTO ent_organizations
                (org_id, identifier_namespace, identifier_value, fact_id, source_id, source_key)
            VALUES (:id, :namespace, :value, :fact_id, :source_id, :source_key)
        """), {"id": org_id, "namespace": reg.identifier.namespace, "value": reg.identifier.value,
                "fact_id": reg.ref.fact_id, "source_id": source.source_id, "source_key": source.source_key})
    else:
        org_id = head.org_id

    version = 1 if head is None else head.version + 1
    recorded_at = conn.execute(text("""
        INSERT INTO ent_organization_versions
            (org_id, version, fact_version, recorded_at, name, revision_ref, snapshot_ref,
             raw_sha256, name_evidence_ref, identifier_path, identifier_evidence_ref)
        VALUES (:org_id, :version, :fact_version,
                GREATEST(clock_timestamp(), CAST(:prior AS timestamptz) + interval '1 microsecond'),
                :name, :revision, :snapshot, :hash, :name_ref, :identifier_path, :identifier_ref)
        RETURNING recorded_at
    """), {"org_id": org_id, "version": version, "fact_version": reg.ref.version,
            "prior": head.recorded_at if head else None, "name": reg.name.value, "revision": reg.revision_ref,
            "snapshot": source.snapshot_ref, "hash": source.raw_sha256, "name_ref": reg.name.evidence_ref,
            "identifier_path": reg.identifier_field.path,
            "identifier_ref": reg.identifier_field.evidence_ref}).scalar_one()
    return OrganizationVersion(
        org_id, version, recorded_at, reg.identifier, reg.name.value, reg.ref,
        reg.revision_ref, reg.source, (reg.name, reg.identifier_field),
    )
