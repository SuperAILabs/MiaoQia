"""Persist deferred live observations in source receipt order."""
from alembic import op

revision = "0006"
down_revision = "0005"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE evd_pending_live_events (
    event_id text PRIMARY KEY,
    source_id text NOT NULL,
    receipt_seq bigint GENERATED ALWAYS AS IDENTITY,
    envelope jsonb NOT NULL
);
CREATE UNIQUE INDEX evd_pending_live_events_source_order ON evd_pending_live_events (source_id, receipt_seq);
""")


def downgrade():
    op.execute("""
LOCK TABLE evd_pending_live_events IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM evd_pending_live_events) THEN
        RAISE EXCEPTION 'drain pending evidence events before downgrading';
    END IF;
END $$;
DROP TABLE evd_pending_live_events;
""")
