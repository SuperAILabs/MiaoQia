"""不可变参考内容和L3增强对象登记；正文/结果仍在对象存储。"""
from alembic import op
revision = "0002"
down_revision = "0001"
branch_labels = None
depends_on = None

def upgrade():
    op.execute("""
CREATE TABLE evd_reference_data (
 name text NOT NULL, version text NOT NULL, object_ref text NOT NULL, content_hash text NOT NULL,
 rights_tags jsonb NOT NULL, PRIMARY KEY(name,version)
);
CREATE TABLE evd_enrichments (
 run_key text PRIMARY KEY, snapshot_ref text NOT NULL REFERENCES evd_snapshots(id),
 revision_ref text NOT NULL REFERENCES evd_extraction_revisions(id), recipe text NOT NULL,
 object_ref text NOT NULL, content_hash text NOT NULL, rights_requirements jsonb NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT now(), UNIQUE(snapshot_ref,revision_ref,recipe)
);
CREATE TABLE evd_derivations (
 id text PRIMARY KEY, fingerprint text NOT NULL UNIQUE, object_ref text NOT NULL, content_hash text NOT NULL,
 rights_requirements jsonb NOT NULL
);
CREATE TRIGGER evd_reference_data_immutable BEFORE UPDATE OR DELETE ON evd_reference_data
 FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_enrichments_immutable BEFORE UPDATE OR DELETE ON evd_enrichments
 FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_derivations_immutable BEFORE UPDATE OR DELETE ON evd_derivations
 FOR EACH ROW EXECUTE FUNCTION evd_immutable();
""")

def downgrade():
    op.execute("""DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM evd_enrichments) OR EXISTS (SELECT 1 FROM evd_derivations)
 OR EXISTS (SELECT 1 FROM evd_reference_data) THEN
 RAISE EXCEPTION '拒绝有损回退：存在增强或参考数据'; END IF;
 END $$;""")
    op.execute("DROP TABLE evd_derivations; DROP TABLE evd_enrichments; DROP TABLE evd_reference_data;")
