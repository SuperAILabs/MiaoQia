"""Durable window leases and shared download pacing."""
from alembic import op

revision = "0008"
down_revision = "0007"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
ALTER TABLE acq_collection_runs ADD COLUMN generation integer NOT NULL DEFAULT 0 CHECK (generation >= 0);
CREATE INDEX acq_collection_runs_recovery ON acq_collection_runs (source_id, mode, id)
    WHERE finished_at IS NULL AND generation > 0;
ALTER TABLE acq_sources
    ADD COLUMN active_run_id bigint REFERENCES acq_collection_runs,
    ADD COLUMN lease_generation integer CHECK (lease_generation > 0),
    ADD COLUMN lease_until timestamptz,
    ADD CHECK ((active_run_id IS NULL) = (lease_generation IS NULL)),
    ADD CHECK ((active_run_id IS NULL) = (lease_until IS NULL));

CREATE TABLE acq_download_pacing (
    scope text PRIMARY KEY,
    sent_at timestamptz NOT NULL,
    ready_at timestamptz NOT NULL CHECK (ready_at >= sent_at)
);
CREATE TABLE acq_download_starts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id bigint NOT NULL REFERENCES acq_collection_runs,
    generation integer NOT NULL CHECK (generation > 0),
    fingerprint text NOT NULL CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    site text NOT NULL,
    dispatched_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX acq_download_starts_run ON acq_download_starts (run_id, generation, id);
ALTER TABLE acq_fetch_attempts ADD COLUMN download_id bigint UNIQUE REFERENCES acq_download_starts;
""")


def downgrade():
    op.execute("""
ALTER TABLE acq_fetch_attempts DROP COLUMN download_id;
DROP TABLE acq_download_starts, acq_download_pacing;
ALTER TABLE acq_sources DROP COLUMN active_run_id, DROP COLUMN lease_generation, DROP COLUMN lease_until;
ALTER TABLE acq_collection_runs DROP COLUMN generation;
""")
