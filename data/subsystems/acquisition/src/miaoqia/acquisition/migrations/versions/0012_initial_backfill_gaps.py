"""Retain unfinished initial backfill days independently of the scan cursor."""

from alembic import op

revision = "0012"
down_revision = "0011"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        CREATE TABLE acq_initial_backfill_gaps (
            source_id text NOT NULL REFERENCES acq_initial_backfill(source_id),
            day date NOT NULL,
            last_run_id bigint NOT NULL REFERENCES acq_collection_runs(id),
            attempted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
            PRIMARY KEY (source_id, day)
        );
    """)


def downgrade():
    # An old worker only knows the scan cursor. Put all outstanding days back
    # within its scan range before dropping the separate retry ledger.
    op.execute("""
        UPDATE acq_initial_backfill b SET next_day = GREATEST(b.next_day, gaps.newest)
        FROM (SELECT source_id, max(day) AS newest FROM acq_initial_backfill_gaps GROUP BY source_id) gaps
        WHERE b.source_id = gaps.source_id;
        DROP TABLE acq_initial_backfill_gaps;
    """)
