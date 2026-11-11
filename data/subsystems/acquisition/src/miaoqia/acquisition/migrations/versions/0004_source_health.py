"""Current source health per source (acquisition.md section 4, SourceHealth).

One row per source holds the state from its latest collection run; changes are published as
acquisition.source.health_changed.v1 in the same transaction.

Revision ID: 0004
"""
from alembic import op

revision = "0004"
down_revision = "0003"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE acq_source_health (
    source_id       text PRIMARY KEY REFERENCES acq_sources,
    state           text NOT NULL CHECK (state IN ('healthy', 'degraded', 'unavailable')),
    reason_code     text NOT NULL CHECK (reason_code <> ''),
    checked_at      timestamptz NOT NULL,
    last_success_at timestamptz,
    last_run        jsonb NOT NULL CHECK (jsonb_typeof(last_run) = 'object')
);
""")


def downgrade():
    op.execute("DROP TABLE acq_source_health;")
