"""Keep page discovery receipts after the response bytes expire."""
from alembic import op

revision = "0007"
down_revision = "0006"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE acq_discovery_pages (
    attempt_id bigint PRIMARY KEY REFERENCES acq_fetch_attempts,
    run_id bigint NOT NULL REFERENCES acq_collection_runs,
    channel text NOT NULL,
    window_start date NOT NULL,
    window_end date NOT NULL CHECK (window_end >= window_start),
    page integer NOT NULL CHECK (page > 0),
    response_hash text NOT NULL CHECK (response_hash ~ '^sha256:[0-9a-f]{64}$'),
    refs jsonb NOT NULL CHECK (jsonb_typeof(refs) = 'array'),
    has_next boolean NOT NULL,
    object_ref text CHECK (object_ref LIKE 'discovery/%'),
    expires_at timestamptz,
    expired_at timestamptz,
    CHECK ((object_ref IS NULL) = (expires_at IS NULL))
);
CREATE INDEX acq_discovery_pages_run ON acq_discovery_pages (run_id, channel, window_start, page);
CREATE INDEX acq_discovery_pages_expiry ON acq_discovery_pages (expires_at, attempt_id)
    WHERE expires_at IS NOT NULL AND expired_at IS NULL;

ALTER TABLE acq_collection_runs ADD COLUMN mode text NOT NULL DEFAULT 'manual'
    CHECK (mode IN ('manual', 'incremental', 'backfill'));
CREATE TABLE acq_initial_backfill (
    source_id text PRIMARY KEY REFERENCES acq_sources,
    oldest_day date NOT NULL,
    next_day date,
    CHECK (next_day >= oldest_day)
);
""")


def downgrade():
    op.execute("""
DROP TABLE acq_initial_backfill, acq_discovery_pages;
ALTER TABLE acq_collection_runs DROP COLUMN mode;
""")
