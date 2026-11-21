"""Track the latest accepted evidence snapshot even when its facts are unchanged."""
from alembic import op

revision = "0005"
down_revision = "0004"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE dom_snapshot_progress (
    info_domain text NOT NULL,
    source_id text NOT NULL,
    source_key text NOT NULL,
    snapshot_seq bigint NOT NULL CHECK (snapshot_seq > 0),
    PRIMARY KEY (info_domain, source_id, source_key)
)
""")


def downgrade():
    op.execute("DROP TABLE dom_snapshot_progress")
