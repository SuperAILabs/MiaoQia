"""Persist request intentions and bind every dispatched attempt to its logical task."""
from alembic import op

revision = "0011"
down_revision = "0010"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        ALTER TABLE acq_collection_runs ADD COLUMN pending_report jsonb
            CHECK (pending_report IS NULL OR (jsonb_typeof(pending_report) = 'object' AND finished_at IS NULL));
        CREATE TABLE acq_request_tasks (
            task_key text PRIMARY KEY CHECK (task_key ~ '^[0-9a-f]{64}$'),
            run_id bigint NOT NULL REFERENCES acq_collection_runs,
            kind text NOT NULL CHECK (kind IN ('list', 'body', 'attachment')),
            scope jsonb NOT NULL CHECK (jsonb_typeof(scope) = 'array'),
            fingerprint text NOT NULL CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
            state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'staged', 'settled', 'failed')),
            last_attempt bigint REFERENCES acq_fetch_attempts,
            updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
        );
        CREATE INDEX acq_request_tasks_run_state ON acq_request_tasks (run_id, state);
        ALTER TABLE acq_download_starts ADD COLUMN task_key text REFERENCES acq_request_tasks;
    """)


def downgrade():
    op.execute("ALTER TABLE acq_download_starts DROP COLUMN task_key")
    op.execute("DROP TABLE acq_request_tasks")
    op.execute("ALTER TABLE acq_collection_runs DROP COLUMN pending_report")
