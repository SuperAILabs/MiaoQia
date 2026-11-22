"""Current-rights PostgreSQL authority; shared state lock defines coherent reads and mutation order."""

from __future__ import annotations

from contextlib import contextmanager
import json
import re

from sqlalchemy import text

from miaoqia.acquisition.api import SourceInput, identifier, policy
from miaoqia.kernel import db, ids


PUBLIC_COMMANDS = {
    "revoke_public": {"action", "source_id", "expected_epoch"},
    "restore_public": {"action", "source_id", "expected_epoch", "evidence_hash"},
}
_POLICY_COMMAND = text("SELECT acq_rights.command(CAST(:cmd AS jsonb),:actor,:reason)")
_PUBLIC_COMMAND = text(
    "SELECT acq_rights.public_source_command(CAST(:cmd AS jsonb),:actor,:reason,:event_id)"
)


class Registry:
    def __init__(self, dsn):
        self.database = db.Database(dsn)
        # A reader that waited for a writer must see the newly committed epoch and rights.
        self.database.engine.update_execution_options(isolation_level="READ COMMITTED")

    def ensure_reader(self):
        with self.database.tx() as c:
            unsafe = c.execute(
                text(
                    """SELECT r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls
                OR has_function_privilege(current_user,'acq_rights.command(jsonb,text,text)','EXECUTE')
                OR has_function_privilege(current_user,'acq_rights.public_source_command(jsonb,text,text,text)','EXECUTE')
                OR NOT has_table_privilege(current_user,'acq_rights.public_source_state','SELECT')
                OR NOT has_table_privilege(current_user,'public.acq_sources','SELECT')
                OR NOT has_schema_privilege(current_user,'public','USAGE')
                OR has_schema_privilege(current_user,'acq_rights','CREATE')
                OR has_schema_privilege(current_user,'public','CREATE')
                OR has_database_privilege(current_user,current_database(),'TEMP')
                OR EXISTS(SELECT 1 FROM pg_auth_members m WHERE m.member = r.oid)
                OR EXISTS(
                    SELECT 1 FROM pg_class t JOIN pg_namespace n ON n.oid = t.relnamespace
                    WHERE t.relkind IN ('r','p') AND n.nspname IN ('acq_rights','public')
                      AND (has_table_privilege(current_user,t.oid,'DELETE,TRUNCATE,TRIGGER')
                        OR has_any_column_privilege(current_user,t.oid,'INSERT,UPDATE'))
                )
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
        return self.command_batch([cmd], actor, reason)[0]

    def command_batch(self, commands, actor, reason):
        """Validate and apply the whole batch in one transaction."""
        for cmd in commands:
            validate_command(cmd)
        with self.database.tx() as c:
            c.execute(text("SET LOCAL statement_timeout = '3000ms'"))
            epochs = []
            for cmd in commands:
                statement = _POLICY_COMMAND
                params = {"cmd": json.dumps(cmd), "actor": actor, "reason": reason}
                if cmd["action"] in PUBLIC_COMMANDS:
                    statement = _PUBLIC_COMMAND
                    params["event_id"] = ids.new("evt")
                epochs.append(c.execute(statement, params).scalar_one())
            return epochs


class Snapshot:
    def __init__(self, conn, epoch):
        self.conn, self.epoch = conn, epoch

    def current_time(self):
        return self.conn.execute(text("SELECT clock_timestamp()")).scalar_one()

    def public_revocations(self, source_ids):
        """Map registered sources to their current source-wide revocation status."""
        return dict(self.conn.execute(
            text("""SELECT s.source_id, COALESCE(r.revoked, false)
              FROM public.acq_sources s LEFT JOIN acq_rights.public_source_state r USING (source_id)
              WHERE s.source_id = ANY(:sources)"""),
            {"sources": sorted(set(source_ids))},
        ).all())

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
        **PUBLIC_COMMANDS,
    }
    if not isinstance(action, str) or action not in expected or set(cmd) != expected[action]:
        raise ValueError("invalid command fields")
    if action in PUBLIC_COMMANDS:
        identifier(cmd["source_id"])
        if type(cmd["expected_epoch"]) is not int or not 0 <= cmd["expected_epoch"] < 2**63:
            raise ValueError("invalid expected epoch")
        if action == "restore_public":
            evidence = cmd["evidence_hash"]
            if not isinstance(evidence, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", evidence):
                raise ValueError("invalid restoration evidence")
    elif action == "register":
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
