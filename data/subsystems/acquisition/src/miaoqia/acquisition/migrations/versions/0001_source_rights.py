"""Acquisition-owned immutable original grants and live revocation. No source activation.

Revision ID: 0001
"""

from alembic import op

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
DO $$ BEGIN EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC',current_database()); END $$;
CREATE SCHEMA acq_rights;
REVOKE ALL ON SCHEMA acq_rights FROM PUBLIC;
CREATE TABLE acq_rights.versions (
 entry_id text NOT NULL, version integer NOT NULL CHECK(version > 0), source_id text NOT NULL,
 policy jsonb NOT NULL CHECK(jsonb_typeof(policy)='object'), PRIMARY KEY(entry_id,version));
CREATE TABLE acq_rights.reviewed (
 source_id text NOT NULL, tags jsonb NOT NULL, PRIMARY KEY(source_id,tags));
INSERT INTO acq_rights.reviewed VALUES
 ('official-qzygjy','["corpus:official-building-tender-20261026","public-official-review"]'),
 ('official-qzygjy','["corpus:official-building-award-20261026","public-official-review"]');
CREATE TABLE acq_rights.aliases (
 source_id text NOT NULL, tags jsonb NOT NULL, entry_id text NOT NULL, version integer NOT NULL,
 PRIMARY KEY(source_id,tags), FOREIGN KEY(source_id,tags) REFERENCES acq_rights.reviewed,
 FOREIGN KEY(entry_id,version) REFERENCES acq_rights.versions);
CREATE TABLE acq_rights.current (
 entry_id text PRIMARY KEY, version integer NOT NULL, revoked boolean NOT NULL,
 FOREIGN KEY(entry_id,version) REFERENCES acq_rights.versions);
CREATE TABLE acq_rights.state (singleton boolean PRIMARY KEY CHECK(singleton), epoch bigint NOT NULL CHECK(epoch >= 0));
INSERT INTO acq_rights.state VALUES(true,0);
CREATE TABLE acq_rights.audit (
 epoch bigint PRIMARY KEY, actor text NOT NULL CHECK(length(actor) BETWEEN 1 AND 96),
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 256), db_actor text NOT NULL,
 at timestamptz NOT NULL DEFAULT clock_timestamp(), command jsonb NOT NULL, old_state jsonb, new_state jsonb);
CREATE FUNCTION acq_rights.immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'immutable rights record' USING ERRCODE='integrity_constraint_violation'; END $$;
CREATE TRIGGER versions_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_rights.versions FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
CREATE TRIGGER aliases_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_rights.aliases FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
CREATE TRIGGER reviewed_immutable BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON acq_rights.reviewed FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
CREATE TRIGGER audit_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_rights.audit FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
CREATE FUNCTION acq_rights.command(cmd jsonb, actor text, reason text) RETURNS bigint
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,acq_rights AS $$
DECLARE e bigint; v jsonb; old_v jsonb; new_v jsonb; p jsonb; src text;
BEGIN
 IF actor IS NULL OR actor !~ '^[a-zA-Z0-9_-]{1,96}$' OR reason IS NULL OR length(reason) NOT BETWEEN 1 AND 256 THEN
  RAISE EXCEPTION 'invalid audit metadata'; END IF;
 PERFORM pg_advisory_xact_lock(682039154);
 SELECT epoch INTO e FROM acq_rights.state WHERE singleton FOR UPDATE;
 IF cmd->>'action' = 'register' THEN
  p := cmd->'policy';
  IF p->>'schema_version' <> '1' OR p->>'entry_id' IS NULL OR p->>'source_id' IS NULL THEN RAISE EXCEPTION 'invalid policy'; END IF;
  SELECT source_id INTO src FROM acq_rights.versions WHERE entry_id=p->>'entry_id' LIMIT 1;
  IF src IS NOT NULL AND src <> p->>'source_id' THEN RAISE EXCEPTION 'source mismatch'; END IF;
  SELECT policy INTO v FROM acq_rights.versions WHERE entry_id=p->>'entry_id' AND version=(p->>'version')::integer;
  IF v IS NOT NULL THEN
   IF v = p THEN RETURN e; END IF; RAISE EXCEPTION 'immutable policy conflict'; END IF;
  INSERT INTO acq_rights.versions VALUES(p->>'entry_id',(p->>'version')::integer,p->>'source_id',p);
  new_v := p;
 ELSIF cmd->>'action' = 'bind' THEN
  SELECT source_id INTO src FROM acq_rights.versions WHERE entry_id=cmd->>'entry_id' AND version=(cmd->>'version')::integer;
  IF src IS NULL OR src <> cmd->>'source_id' THEN RAISE EXCEPTION 'source mismatch'; END IF;
  SELECT to_jsonb(a) INTO v FROM acq_rights.aliases a WHERE source_id=src AND tags=cmd->'tags';
  IF v IS NOT NULL THEN
   IF v->>'entry_id'=cmd->>'entry_id' AND v->>'version'=cmd->>'version' THEN RETURN e; END IF;
   RAISE EXCEPTION 'immutable alias conflict'; END IF;
  INSERT INTO acq_rights.aliases VALUES(src,cmd->'tags',cmd->>'entry_id',(cmd->>'version')::integer);
  new_v := cmd;
 ELSIF cmd->>'action' IN ('activate','revoke') THEN
  SELECT to_jsonb(c) INTO old_v FROM acq_rights.current c WHERE entry_id=cmd->>'entry_id';
  IF cmd->>'action'='activate' THEN
   IF NOT EXISTS(SELECT 1 FROM acq_rights.versions WHERE entry_id=cmd->>'entry_id' AND version=(cmd->>'version')::integer) THEN RAISE EXCEPTION 'unknown version'; END IF;
   new_v := jsonb_build_object('entry_id',cmd->>'entry_id','version',(cmd->>'version')::integer,'revoked',false);
  ELSE
   IF old_v IS NULL THEN RAISE EXCEPTION 'unknown entry'; END IF;
   new_v := old_v || jsonb_build_object('revoked',true);
  END IF;
  IF new_v=old_v THEN RETURN e; END IF;
  INSERT INTO acq_rights.current VALUES(new_v->>'entry_id',(new_v->>'version')::integer,(new_v->>'revoked')::boolean)
   ON CONFLICT(entry_id) DO UPDATE SET version=EXCLUDED.version,revoked=EXCLUDED.revoked;
 ELSE RAISE EXCEPTION 'invalid command'; END IF;
 e := e+1;
 UPDATE acq_rights.state SET epoch=e WHERE singleton;
 INSERT INTO acq_rights.audit(epoch,actor,reason,db_actor,command,old_state,new_state)
  VALUES(e,actor,reason,session_user,cmd,old_v,new_v);
 RETURN e;
END $$;
REVOKE ALL ON ALL TABLES IN SCHEMA acq_rights FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA acq_rights FROM PUBLIC;
""")


def downgrade():
    raise RuntimeError("source rights/audit history cannot be destructively downgraded")
