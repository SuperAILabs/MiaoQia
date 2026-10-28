"""Immutable closure and single finite owner-approved reference binding."""
import hashlib
import json
from importlib import resources
from alembic import op
revision = "0003"
down_revision = "0002"
branch_labels = depends_on = None

def upgrade():
    manifest = json.loads(resources.files("miaoqia.evidence").joinpath("resources/reference_manifest.json").read_text())
    manifest_hash = "sha256:" + hashlib.sha256(json.dumps(manifest, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()).hexdigest()
    # This migration's finite approval is pinned; changes require a new migration.
    if manifest != {"schema_version": 1, "references": [{"name": "proc-building-topic", "version": "0.1.0", "content_hash": "sha256:2b5aef8fb787639360a27918f62ee56c4fb1c573f0c564ed288e1660fd13e258", "rights_tags": ["review:building-topic-pair"], "source_kind": "reference", "source_id": "reference-proc-building-topic-v0-1"}]}:
        raise ValueError("reviewed manifest changed")
    op.execute("""
CREATE TABLE evd_requirement_sets (
 kind text NOT NULL CHECK(kind IN ('derivation','enrichment')), key text NOT NULL,
 subject_hash text NOT NULL, closure jsonb NOT NULL, proof_hash text NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT now(), actor text NOT NULL, reason text NOT NULL,
 db_actor text NOT NULL DEFAULT session_user, PRIMARY KEY(kind,key));
CREATE FUNCTION evd_requirement_audit() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.actor !~ '^[a-zA-Z0-9_-]{1,96}$' OR length(NEW.reason) NOT BETWEEN 1 AND 256 OR NEW.reason ~ '[[:cntrl:]]' THEN
 RAISE EXCEPTION 'invalid audit metadata'; END IF;
 NEW.db_actor=session_user; NEW.recorded_at=clock_timestamp(); RETURN NEW;
END $$;
CREATE TRIGGER evd_requirements_audit BEFORE INSERT ON evd_requirement_sets
 FOR EACH ROW EXECUTE FUNCTION evd_requirement_audit();
CREATE TRIGGER evd_requirements_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON evd_requirement_sets
 FOR EACH STATEMENT EXECUTE FUNCTION evd_immutable();
CREATE SCHEMA evd_provenance;
REVOKE ALL ON SCHEMA evd_provenance FROM PUBLIC;
CREATE TABLE evd_provenance.manifest (
 name text NOT NULL, version text NOT NULL, content_hash text NOT NULL, rights_tags jsonb NOT NULL,
 source_kind text NOT NULL CHECK(source_kind='reference'), source_id text NOT NULL, manifest_hash text NOT NULL,
 PRIMARY KEY(name,version));
INSERT INTO evd_provenance.manifest VALUES
 ('proc-building-topic','0.1.0','sha256:2b5aef8fb787639360a27918f62ee56c4fb1c573f0c564ed288e1660fd13e258',
 '["review:building-topic-pair"]','reference','reference-proc-building-topic-v0-1',
 '__MANIFEST_HASH__');
CREATE TABLE evd_provenance.bindings (
 name text NOT NULL, version text NOT NULL, content_hash text NOT NULL, rights_tags jsonb NOT NULL,
 source_kind text NOT NULL CHECK(source_kind='reference'), source_id text NOT NULL, manifest_hash text NOT NULL,
 actor text NOT NULL, reason text NOT NULL, db_actor text NOT NULL, bound_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(name,version), FOREIGN KEY(name,version) REFERENCES evd_provenance.manifest);
CREATE TRIGGER evd_manifest_immutable BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON evd_provenance.manifest
 FOR EACH STATEMENT EXECUTE FUNCTION public.evd_immutable();
CREATE TRIGGER evd_bindings_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON evd_provenance.bindings
 FOR EACH STATEMENT EXECUTE FUNCTION public.evd_immutable();
CREATE FUNCTION evd_provenance.bind_reference(n text,v text,a text,r text) RETURNS void
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE m evd_provenance.manifest%ROWTYPE; ref public.evd_reference_data%ROWTYPE; b evd_provenance.bindings%ROWTYPE;
BEGIN
 IF a IS NULL OR a !~ '^[a-zA-Z0-9_-]{1,96}$' OR r IS NULL OR length(r) NOT BETWEEN 1 AND 256 OR r ~ '[[:cntrl:]]' THEN
  RAISE EXCEPTION 'invalid audit metadata'; END IF;
 SELECT * INTO m FROM evd_provenance.manifest WHERE name=n AND version=v;
 IF NOT FOUND THEN RAISE EXCEPTION 'reference not reviewed'; END IF;
 PERFORM pg_advisory_xact_lock(hashtext('reference-binding:'||n||':'||v));
 SELECT * INTO ref FROM public.evd_reference_data WHERE name=n AND version=v;
 IF NOT FOUND OR ref.content_hash<>m.content_hash OR ref.rights_tags<>m.rights_tags THEN
  RAISE EXCEPTION 'reference identity mismatch'; END IF;
 SELECT * INTO b FROM evd_provenance.bindings WHERE name=n AND version=v;
 IF FOUND THEN
  IF b.content_hash<>m.content_hash OR b.rights_tags<>m.rights_tags OR b.source_id<>m.source_id OR b.manifest_hash<>m.manifest_hash THEN
   RAISE EXCEPTION 'binding conflict'; END IF;
  RETURN;
 END IF;
 INSERT INTO evd_provenance.bindings SELECT m.*,a,r,session_user,clock_timestamp();
END $$;
REVOKE ALL ON ALL TABLES IN SCHEMA evd_provenance FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA evd_provenance FROM PUBLIC;
-- Global owner defaults can grant explicit DML even in a new schema. Remove it.
DO $$ DECLARE g record; BEGIN
 FOR g IN SELECT DISTINCT grantee FROM information_schema.table_privileges
   WHERE table_schema='evd_provenance' AND grantee NOT IN (current_user,'PUBLIC') LOOP
  EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA evd_provenance FROM %I',g.grantee);
 END LOOP;
 -- Function defaults can also create direct EXECUTE grants, independently of PUBLIC.
 FOR g IN SELECT DISTINCT r.rolname AS grantee FROM pg_proc p
   JOIN pg_namespace n ON n.oid=p.pronamespace
   CROSS JOIN LATERAL aclexplode(p.proacl) a JOIN pg_roles r ON r.oid=a.grantee
   WHERE n.nspname='evd_provenance' AND r.rolname<>current_user LOOP
  EXECUTE format('REVOKE ALL ON ALL FUNCTIONS IN SCHEMA evd_provenance FROM %I',g.grantee);
 END LOOP;
 -- Existing evidence consumers need metadata SELECT; no binding mutation privilege.
 FOR g IN SELECT DISTINCT grantee FROM information_schema.table_privileges
   WHERE table_schema='public' AND table_name='evd_reference_data' AND privilege_type='SELECT'
     AND grantee NOT IN (current_user,'PUBLIC') LOOP
  EXECUTE format('GRANT USAGE ON SCHEMA evd_provenance TO %I',g.grantee);
  EXECUTE format('GRANT SELECT ON ALL TABLES IN SCHEMA evd_provenance TO %I',g.grantee);
 END LOOP;
END $$;
""".replace("__MANIFEST_HASH__", manifest_hash))

def downgrade():
    op.execute("""DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM evd_requirement_sets) OR EXISTS(SELECT 1 FROM evd_provenance.bindings) THEN
 RAISE EXCEPTION '拒绝有损回退: provenance audit exists'; END IF;
 END $$; DROP SCHEMA evd_provenance CASCADE; DROP TABLE evd_requirement_sets; DROP FUNCTION evd_requirement_audit();""")
