"""Current-rights PostgreSQL authority; shared state lock defines coherent reads and mutation order."""

from __future__ import annotations

from contextlib import contextmanager
import json

from sqlalchemy import text

from miaoqia.acquisition.api import SourceInput, identifier, policy
from miaoqia.kernel import db


class Registry:
    def __init__(self, dsn):
        self.database = db.Database(dsn)

    def ensure_reader(self):
        with self.database.tx() as c:
            unsafe = c.execute(
                text(
                    """SELECT r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls
                OR has_function_privilege(current_user,'acq_rights.command(jsonb,text,text)','EXECUTE')
                OR has_schema_privilege(current_user,'acq_rights','CREATE')
                OR has_schema_privilege(current_user,'public','CREATE')
                OR has_database_privilege(current_user,current_database(),'TEMP')
                OR EXISTS(SELECT 1 FROM pg_tables t WHERE t.schemaname IN ('acq_rights','public')
                    AND has_table_privilege(current_user,quote_ident(t.schemaname)||'.'||quote_ident(t.tablename),'INSERT,UPDATE,DELETE,TRUNCATE'))
                FROM pg_roles r WHERE r.rolname=current_user"""
                )
            ).scalar_one()
            if unsafe:
                raise PermissionError("dedicated read-only role required")

    def close(self):
        self.database.close()

    @contextmanager
    def snapshot(self):
        # A shared transaction advisory lock orders authorization against operator mutation.
        # No caller receives a DB transaction/lock or acquisition credentials.
        with self.database.tx() as c:
            c.execute(text("SET LOCAL statement_timeout = '3000ms'"))
            c.execute(text("SELECT pg_advisory_xact_lock_shared(682039154)"))
            epoch = c.execute(
                text("SELECT epoch FROM acq_rights.state WHERE singleton")
            ).scalar_one()
            yield Snapshot(c, epoch)

    def command(self, cmd, actor, reason):
        validate_command(cmd)
        with self.database.tx() as c:
            c.execute(text("SET LOCAL statement_timeout = '3000ms'"))
            return c.execute(
                text("SELECT acq_rights.command(CAST(:cmd AS jsonb),:actor,:reason)"),
                {"cmd": json.dumps(cmd), "actor": actor, "reason": reason},
            ).scalar_one()


class Snapshot:
    def __init__(self, conn, epoch):
        self.conn, self.epoch = conn, epoch

    def current_time(self):
        return self.conn.execute(text("SELECT clock_timestamp()")).scalar_one()

    def versions(self, source):
        row = self.conn.execute(
            text("""SELECT v.policy,cur.policy,c.revoked
          FROM acq_rights.aliases a JOIN acq_rights.versions v USING(entry_id,version)
          JOIN acq_rights.current c ON c.entry_id=a.entry_id
          JOIN acq_rights.versions cur ON cur.entry_id=c.entry_id AND cur.version=c.version
          WHERE a.source_id=:s AND a.tags=CAST(:t AS jsonb)"""),
            {"s": source.source_id, "t": json.dumps(source.tags)},
        ).one_or_none()
        if row is None or row[2]:
            return None
        return tuple(policy(p) for p in row[:2])


def validate_command(cmd):
    if not isinstance(cmd, dict):
        raise ValueError("invalid command")
    action = cmd.get("action")
    expected = {
        "register": {"action", "policy"},
        "bind": {"action", "source_id", "tags", "entry_id", "version"},
        "activate": {"action", "entry_id", "version"},
        "revoke": {"action", "entry_id"},
    }
    if action not in expected or set(cmd) != expected[action]:
        raise ValueError("invalid command fields")
    if action == "register":
        policy(cmd["policy"])
    else:
        identifier(cmd["entry_id"])
        if action != "revoke" and (
            type(cmd["version"]) is not int or cmd["version"] < 1
        ):
            raise ValueError("invalid version")
        if action == "bind":
            parsed = SourceInput.parse(
                {"source_id": cmd["source_id"], "tags": cmd["tags"]}
            )
            if tuple(cmd["tags"]) != parsed.tags:
                raise ValueError("tags must be canonical sorted exact combination")
