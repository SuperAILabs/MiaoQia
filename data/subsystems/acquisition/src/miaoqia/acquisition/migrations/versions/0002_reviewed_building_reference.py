"""Owner-only finite reference eligibility; no aliases, permits or activation."""
from alembic import op
revision = "0002"
down_revision = "0001"
branch_labels = depends_on = None

def upgrade():
    op.execute("""
LOCK TABLE acq_rights.reviewed IN ACCESS EXCLUSIVE MODE;
DROP TRIGGER reviewed_immutable ON acq_rights.reviewed;
INSERT INTO acq_rights.reviewed VALUES
 ('reference-proc-building-topic-v0-1','["review:building-topic-pair"]');
CREATE TRIGGER reviewed_immutable BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON acq_rights.reviewed
 FOR EACH STATEMENT EXECUTE FUNCTION acq_rights.immutable();
""")

def downgrade():
    raise RuntimeError("reviewed provenance cannot be destructively downgraded")
