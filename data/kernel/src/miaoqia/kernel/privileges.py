"""Owner provisioning validation shared by restricted offline database capabilities."""


def independent_roles(connection, names):
    """Require distinct existing unprivileged roles without inherited memberships."""
    owner = connection.execute("SELECT current_user").fetchone()[0]
    if len(set(names)) != len(names):
        raise ValueError("distinct roles required")
    for name in names:
        role = connection.execute(
            "SELECT rolsuper,rolcreaterole,rolcreatedb,rolbypassrls FROM pg_roles WHERE rolname=%s", (name,)
        ).fetchone()
        if (role is None or any(role) or name == owner or connection.execute(
            "SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname=%s", (name,)
        ).fetchone()):
            raise ValueError("unprivileged independent roles required")
    return owner
