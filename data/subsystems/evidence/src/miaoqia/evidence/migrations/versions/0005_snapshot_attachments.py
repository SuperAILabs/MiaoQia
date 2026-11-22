"""Preserve the attachment receipts of each immutable document snapshot."""
from alembic import op

revision = "0005"
down_revision = "0004"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE evd_snapshot_attachments (
    snapshot_id text PRIMARY KEY REFERENCES evd_snapshots,
    receipts jsonb NOT NULL CHECK (jsonb_typeof(receipts) = 'array')
);
CREATE TRIGGER evd_snapshot_attachments_immutable BEFORE UPDATE OR DELETE ON evd_snapshot_attachments
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();
""")


def downgrade():
    op.execute("DROP TABLE evd_snapshot_attachments")
