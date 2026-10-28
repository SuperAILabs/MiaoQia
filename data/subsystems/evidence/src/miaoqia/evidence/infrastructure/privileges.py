"""Owner-only setup of separate evidence reader and finite reference binder roles."""
import psycopg
from psycopg import sql
from miaoqia.kernel.privileges import independent_roles


def provision(owner_dsn, reader, binder):
    with psycopg.connect(owner_dsn) as conn:
        owner = independent_roles(conn, (reader, binder))
        if conn.execute("SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='evd_provenance'").fetchone() != (owner,):
            raise ValueError("evidence schema owner required")
        conn.execute("REVOKE CREATE ON SCHEMA public FROM PUBLIC")
        database = conn.execute("SELECT current_database()").fetchone()[0]
        conn.execute(sql.SQL("REVOKE CREATE,TEMPORARY ON DATABASE {} FROM PUBLIC").format(sql.Identifier(database)))
        conn.execute("REVOKE ALL ON SCHEMA evd_provenance FROM PUBLIC")
        conn.execute("REVOKE ALL ON ALL TABLES IN SCHEMA evd_provenance FROM PUBLIC")
        conn.execute("REVOKE ALL ON ALL FUNCTIONS IN SCHEMA evd_provenance FROM PUBLIC")
        for name in (reader, binder):
            ident = sql.Identifier(name)
            conn.execute(sql.SQL("REVOKE CREATE,TEMPORARY ON DATABASE {} FROM {}").format(sql.Identifier(database), ident))
            for schema in ("public", "evd_provenance"):
                conn.execute(sql.SQL("REVOKE ALL ON ALL TABLES IN SCHEMA {} FROM {}").format(sql.Identifier(schema), ident))
                conn.execute(sql.SQL("REVOKE ALL ON ALL SEQUENCES IN SCHEMA {} FROM {}").format(sql.Identifier(schema), ident))
                conn.execute(sql.SQL("REVOKE ALL ON ALL FUNCTIONS IN SCHEMA {} FROM {}").format(sql.Identifier(schema), ident))
                conn.execute(sql.SQL("REVOKE ALL ON SCHEMA {} FROM {}").format(sql.Identifier(schema), ident))
                conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES IN SCHEMA {} REVOKE ALL ON TABLES FROM {}").format(sql.Identifier(schema), ident))
                conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES IN SCHEMA {} REVOKE ALL ON FUNCTIONS FROM {}").format(sql.Identifier(schema), ident))
                conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES IN SCHEMA {} REVOKE ALL ON SEQUENCES FROM {}").format(sql.Identifier(schema), ident))
            conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES REVOKE ALL ON TABLES FROM {}").format(ident))
            conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES REVOKE ALL ON FUNCTIONS FROM {}").format(ident))
            conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES REVOKE ALL ON SEQUENCES FROM {}").format(ident))
            conn.execute(sql.SQL("ALTER DEFAULT PRIVILEGES REVOKE ALL ON SCHEMAS FROM {}").format(ident))
            conn.execute(sql.SQL("GRANT USAGE ON SCHEMA public,evd_provenance TO {}").format(ident))
        conn.execute(sql.SQL("GRANT SELECT ON evd_snapshots,evd_extraction_revisions,evd_evidence_refs,evd_reference_data,evd_derivations,evd_enrichments,evd_requirement_sets,evd_provenance.manifest,evd_provenance.bindings TO {}").format(sql.Identifier(reader)))
        conn.execute(sql.SQL("GRANT EXECUTE ON FUNCTION evd_provenance.bind_reference(text,text,text,text) TO {}").format(sql.Identifier(binder)))
