"""Organization search handle enumeration and durable projection revisions."""

from sqlalchemy import text


def for_registration(conn, fact_id):
    return tuple(conn.execute(text("SELECT org_id FROM ent_organizations WHERE fact_id = :id"),
                              {"id": fact_id}).scalars())


def for_source(conn, source_id, after, limit):
    return tuple(conn.execute(text("""
        SELECT org_id FROM ent_organizations
        WHERE source_id = :source AND org_id COLLATE "C" > :after
        ORDER BY org_id COLLATE "C" LIMIT :limit
    """), {"source": source_id, "after": after, "limit": limit}).scalars())


def record(conn, org_id, version, revision, digest):
    conn.execute(text("""
        INSERT INTO ent_search_projection(org_id, public_version, revision, digest)
        VALUES (:id, :version, :revision, :digest)
        ON CONFLICT (org_id) DO UPDATE SET public_version = excluded.public_version,
            revision = excluded.revision, digest = excluded.digest
    """), {"id": org_id, "version": version, "revision": revision, "digest": digest})


def snapshot_rows(conn, *, limit):
    # One PostgreSQL statement gives the timestamp and every handle the same
    # MVCC view, including the one header-only row for an empty owner.
    statement = text("""
        WITH basis AS MATERIALIZED (
            SELECT statement_timestamp() AS as_of, pg_is_in_recovery() AS standby
        )
        SELECT basis.as_of, basis.standby, head.org_id, head.version
        FROM basis LEFT JOIN LATERAL (
            SELECT o.org_id, (
                SELECT max(v.version) FROM ent_organization_versions v WHERE v.org_id = o.org_id
            ) AS version
            FROM ent_organizations o WHERE NOT basis.standby
            ORDER BY o.org_id COLLATE "C" LIMIT :limit
        ) head ON TRUE
        ORDER BY head.org_id COLLATE "C"
    """)
    return conn.execute(statement, {"limit": limit})
