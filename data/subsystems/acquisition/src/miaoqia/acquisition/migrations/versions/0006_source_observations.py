"""Record notice changes independently of content-addressed artifacts."""
from alembic import op

revision = "0006"
down_revision = "0005"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE acq_source_observations (
    source_id       text NOT NULL REFERENCES acq_sources,
    notice_key      text NOT NULL CHECK (notice_key <> ''),
    version         bigint NOT NULL CHECK (version > 0),
    artifact_id     bigint NOT NULL REFERENCES acq_raw_artifacts,
    attempt_id      bigint NOT NULL UNIQUE REFERENCES acq_fetch_attempts,
    source_url      text NOT NULL,
    published_on    date,
    observed_at     timestamptz NOT NULL,
    PRIMARY KEY (source_id, notice_key, version)
);
CREATE TRIGGER acq_source_observations_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_source_observations
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();
CREATE INDEX acq_source_observations_artifact ON acq_source_observations (artifact_id);

CREATE TABLE acq_detail_checks (
    source_id       text NOT NULL,
    notice_key      text NOT NULL,
    version         bigint NOT NULL,
    attempt_id      bigint NOT NULL REFERENCES acq_fetch_attempts,
    legacy_artifact_id bigint REFERENCES acq_raw_artifacts,
    checked_at      timestamptz NOT NULL,
    PRIMARY KEY (source_id, notice_key),
    FOREIGN KEY (source_id, notice_key, version) REFERENCES acq_source_observations
);

CREATE TABLE acq_pending_identity (
    attempt_id bigint PRIMARY KEY REFERENCES acq_fetch_attempts,
    artifact_id bigint NOT NULL REFERENCES acq_raw_artifacts,
    source_url text NOT NULL,
    observed_at timestamptz NOT NULL
);
CREATE TRIGGER acq_pending_identity_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_pending_identity
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();
""")


def downgrade():
    op.execute("DROP TABLE acq_pending_identity, acq_detail_checks, acq_source_observations")
