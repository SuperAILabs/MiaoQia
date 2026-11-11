"""Source registry, fetch attempts, request fingerprints and raw artifact metadata (acquisition.md section 4).

Raw bytes live in the object store under raw/<sha256>; the database keeps metadata only. Attempts and artifacts are
append-only: corrections are new rows, never updates.

Revision ID: 0003
"""
from alembic import op

revision = "0003"
down_revision = "0002"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE acq_sources (
    source_id    text PRIMARY KEY CHECK (source_id ~ '^[a-z][a-z0-9_-]{0,95}$'),
    handler      text NOT NULL,
    access       jsonb NOT NULL CHECK (jsonb_typeof(access) = 'object'),
    state        text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'blocked')),
    state_reason text,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'blocked') = (state_reason IS NOT NULL))
);

CREATE TABLE acq_fetch_attempts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id    text NOT NULL REFERENCES acq_sources,
    fingerprint  text NOT NULL CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    method       text NOT NULL CHECK (method IN ('GET', 'POST')),
    url          text NOT NULL,
    status       integer,
    outcome      text NOT NULL CHECK (outcome IN ('ok', 'throttled', 'blocked', 'http_error', 'network_error', 'invalid')),
    duration_ms  integer NOT NULL CHECK (duration_ms >= 0),
    error        text,
    attempted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX acq_fetch_attempts_source ON acq_fetch_attempts (source_id, attempted_at);

-- Detail requests already archived; incremental runs skip them unless a refetch is requested.
CREATE TABLE acq_requests (
    fingerprint     text PRIMARY KEY CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    source_id       text NOT NULL REFERENCES acq_sources,
    source_key      text NOT NULL CHECK (source_key <> ''),
    content_hash    text NOT NULL CHECK (content_hash ~ '^sha256:[0-9a-f]{64}$'),
    last_fetched_at timestamptz NOT NULL
);

CREATE TABLE acq_raw_artifacts (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id           text NOT NULL REFERENCES acq_sources,
    source_key          text NOT NULL CHECK (source_key <> ''),
    content_hash        text NOT NULL CHECK (content_hash ~ '^sha256:[0-9a-f]{64}$'),
    artifact_ref        text NOT NULL CHECK (artifact_ref ~ '^raw/[0-9a-f]{64}$'),
    media_type          text NOT NULL,
    size_bytes          integer NOT NULL CHECK (size_bytes >= 0),
    source_url          text NOT NULL,
    request_fingerprint text NOT NULL,
    fetched_at          timestamptz NOT NULL,
    UNIQUE (source_id, source_key, content_hash)
);

CREATE FUNCTION acq_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'append-only acquisition record' USING ERRCODE = 'integrity_constraint_violation'; END $$;
CREATE TRIGGER acq_fetch_attempts_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_fetch_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();
CREATE TRIGGER acq_raw_artifacts_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_raw_artifacts
    FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();
""")


def downgrade():
    op.execute("""
DROP TABLE acq_raw_artifacts;
DROP TABLE acq_requests;
DROP TABLE acq_fetch_attempts;
DROP TABLE acq_sources;
DROP FUNCTION acq_append_only();
""")
