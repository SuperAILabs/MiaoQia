"""Immutable fixed-version input closure; permission remains current."""
from alembic import op
revision = "0004"
down_revision = "0003"
branch_labels = depends_on = None

def upgrade():
    op.execute("""
CREATE TABLE dom_version_requirements (
 fact_id text NOT NULL, version integer NOT NULL, subject_hash text NOT NULL,
 closure jsonb NOT NULL, proof_hash text NOT NULL, recorded_at timestamptz NOT NULL DEFAULT now(),
 actor text NOT NULL, reason text NOT NULL, db_actor text NOT NULL DEFAULT session_user,
 PRIMARY KEY(fact_id,version), FOREIGN KEY(fact_id,version) REFERENCES dom_fact_versions(fact_id,version));
CREATE FUNCTION dom_requirement_audit() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.actor !~ '^[a-zA-Z0-9_-]{1,96}$' OR length(NEW.reason) NOT BETWEEN 1 AND 256 OR NEW.reason ~ '[[:cntrl:]]' THEN
 RAISE EXCEPTION 'invalid audit metadata'; END IF;
 NEW.db_actor=session_user; NEW.recorded_at=clock_timestamp(); RETURN NEW;
END $$;
CREATE TRIGGER dom_requirements_audit BEFORE INSERT ON dom_version_requirements
 FOR EACH ROW EXECUTE FUNCTION dom_requirement_audit();
CREATE TRIGGER dom_requirements_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON dom_version_requirements
 FOR EACH STATEMENT EXECUTE FUNCTION dom_versions_immutable();
""")

def downgrade():
    op.execute("""DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM dom_version_requirements) THEN
 RAISE EXCEPTION '拒绝有损回退: source requirement audit exists'; END IF;
 END $$; DROP TABLE dom_version_requirements; DROP FUNCTION dom_requirement_audit();""")
