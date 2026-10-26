"""Owner-only provisioning, distinct existing reader/operator login roles. No automatic role creation."""

import psycopg
from psycopg import sql


def provision(owner_dsn, reader, operator):
    if reader == operator:
        raise ValueError("distinct roles required")
    with psycopg.connect(owner_dsn) as c:
        owner = c.execute("SELECT current_user").fetchone()[0]
        for name in (reader, operator):
            role = c.execute(
                "SELECT rolsuper,rolcreaterole,rolcreatedb,rolbypassrls FROM pg_roles WHERE rolname=%s",
                (name,),
            ).fetchone()
            if (
                role is None
                or any(role)
                or name == owner
                or c.execute(
                    "SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname=%s",
                    (name,),
                ).fetchone()
            ):
                raise ValueError("unprivileged independent roles required")
            ident = sql.Identifier(name)
            # Remove historical public-schema defaults on fixture/operator/runtime accounts.
            c.execute(
                sql.SQL(
                    "REVOKE ALL ON ALL TABLES IN SCHEMA public,acq_rights FROM {}"
                ).format(ident)
            )
            c.execute(
                sql.SQL(
                    "REVOKE ALL ON ALL SEQUENCES IN SCHEMA public,acq_rights FROM {}"
                ).format(ident)
            )
            c.execute(
                sql.SQL(
                    "REVOKE ALL ON ALL FUNCTIONS IN SCHEMA acq_rights FROM {}"
                ).format(ident)
            )
            c.execute(sql.SQL("GRANT USAGE ON SCHEMA acq_rights TO {}").format(ident))
            c.execute(
                sql.SQL("GRANT SELECT ON ALL TABLES IN SCHEMA acq_rights TO {}").format(
                    ident
                )
            )
        c.execute(
            sql.SQL(
                "GRANT EXECUTE ON FUNCTION acq_rights.command(jsonb,text,text) TO {}"
            ).format(sql.Identifier(operator))
        )
