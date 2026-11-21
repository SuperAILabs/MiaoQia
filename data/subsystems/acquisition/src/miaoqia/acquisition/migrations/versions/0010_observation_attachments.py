"""Pin each attachment receipt to a body observation and associate retry attempts."""
from alembic import op

revision = "0010"
down_revision = "0009"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
ALTER TABLE acq_source_observations
    ADD COLUMN channel text,
    DROP CONSTRAINT acq_source_observations_attempt_id_key;
CREATE INDEX acq_source_observations_attempt ON acq_source_observations (attempt_id);
ALTER TABLE acq_raw_artifacts ADD UNIQUE (source_id, id);

CREATE TABLE acq_observation_attachments (
    source_id text NOT NULL,
    notice_key text NOT NULL,
    version bigint NOT NULL,
    part_key text NOT NULL CHECK (length(part_key) BETWEEN 1 AND 512),
    required boolean NOT NULL,
    name text CHECK (length(name) BETWEEN 1 AND 512),
    request_fingerprint text CHECK (request_fingerprint ~ '^sha256:[0-9a-f]{64}$'),
    status text NOT NULL CHECK (status IN ('pending', 'available')),
    artifact_id bigint,
    attempt_id bigint REFERENCES acq_fetch_attempts,
    PRIMARY KEY (source_id, notice_key, version, part_key),
    FOREIGN KEY (source_id, notice_key, version) REFERENCES acq_source_observations,
    FOREIGN KEY (source_id, artifact_id) REFERENCES acq_raw_artifacts (source_id, id),
    CHECK ((status = 'available') = (artifact_id IS NOT NULL)),
    CHECK ((artifact_id IS NULL) = (attempt_id IS NULL))
);
CREATE TRIGGER acq_observation_attachments_append_only BEFORE UPDATE OR DELETE OR TRUNCATE
    ON acq_observation_attachments FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();
CREATE INDEX acq_observation_attachments_pending ON acq_observation_attachments (source_id, notice_key, version)
    WHERE status = 'pending';

ALTER TABLE acq_fetch_attempts
    ADD COLUMN attachment_notice_key text,
    ADD COLUMN attachment_version bigint,
    ADD COLUMN attachment_part_key text,
    ADD CHECK ((attachment_notice_key IS NULL) = (attachment_version IS NULL)
               AND (attachment_notice_key IS NULL) = (attachment_part_key IS NULL)),
    ADD FOREIGN KEY (source_id, attachment_notice_key, attachment_version, attachment_part_key)
        REFERENCES acq_observation_attachments;
CREATE INDEX acq_fetch_attempts_attachment ON acq_fetch_attempts
    (source_id, attachment_notice_key, attachment_version, attachment_part_key, id)
    WHERE attachment_notice_key IS NOT NULL;

CREATE FUNCTION public.acq_collection_allowed(target text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF NOT has_table_privilege(session_user, 'public.acq_sources', 'UPDATE') THEN
        RAISE insufficient_privilege;
    END IF;
    PERFORM pg_advisory_xact_lock_shared(682039154);
    RETURN EXISTS (SELECT 1 FROM public.acq_sources WHERE source_id = target) AND NOT EXISTS (
        SELECT 1 FROM acq_rights.public_source_state WHERE source_id = target AND revoked
    );
END
$$;
""")


def downgrade():
    raise RuntimeError("attachment observation history cannot be destructively downgraded")
