from alembic import op

revision = "0005"
down_revision = "0004"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE acq_collection_runs (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id      text NOT NULL REFERENCES acq_sources,
    window_start   date NOT NULL,
    window_end     date NOT NULL CHECK (window_end >= window_start),
    max_pages      integer NOT NULL CHECK (max_pages >= 1),
    refetch        boolean NOT NULL,
    started_at     timestamptz NOT NULL,
    finished_at    timestamptz,
    report         jsonb CHECK (jsonb_typeof(report) = 'object'),
    CHECK ((finished_at IS NULL) = (report IS NULL))
);
CREATE INDEX acq_collection_runs_source ON acq_collection_runs (source_id, id DESC);
ALTER TABLE acq_fetch_attempts ADD COLUMN run_id bigint REFERENCES acq_collection_runs;
CREATE INDEX acq_fetch_attempts_run ON acq_fetch_attempts (run_id, id) WHERE run_id IS NOT NULL;
""")


def downgrade():
    op.execute("""
ALTER TABLE acq_fetch_attempts DROP COLUMN run_id;
DROP TABLE acq_collection_runs;
""")
