"""固定source-base/parent和可验证evidence结果的不可变应用receipt。"""
from alembic import op
revision = "0003"
down_revision = "0002"
branch_labels = None
depends_on = None

def upgrade():
    op.execute("""
CREATE TABLE dom_enrichment_receipts (
 run_key text PRIMARY KEY, notice_id text NOT NULL, source_base_version integer NOT NULL,
 parent_version integer NOT NULL, output_version integer NOT NULL,
 recipe text NOT NULL, source_hash text NOT NULL, input_hash text NOT NULL, output_hash text NOT NULL,
 reference_hash text NOT NULL, evidence_refs jsonb NOT NULL, outputs jsonb NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(notice_id,source_base_version) REFERENCES dom_fact_versions(fact_id,version),
 FOREIGN KEY(notice_id,parent_version) REFERENCES dom_fact_versions(fact_id,version),
 FOREIGN KEY(notice_id,output_version) REFERENCES dom_fact_versions(fact_id,version)
);
CREATE TRIGGER dom_enrichment_receipts_immutable BEFORE UPDATE OR DELETE ON dom_enrichment_receipts
 FOR EACH ROW EXECUTE FUNCTION dom_versions_immutable();
""")

def downgrade():
    op.execute("""DO $$ BEGIN IF EXISTS(SELECT 1 FROM dom_enrichment_receipts) THEN
      RAISE EXCEPTION '已有增强receipt，拒绝有损回退'; END IF; END $$;
      DROP TABLE dom_enrichment_receipts;""")
