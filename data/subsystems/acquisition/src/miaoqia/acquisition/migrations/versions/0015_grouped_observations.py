"""Bind grouped source observations to one validated original and request receipt."""

from alembic import op

revision = "0015"
down_revision = "0014"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        ALTER TABLE acq_request_tasks DROP CONSTRAINT acq_request_tasks_kind_check,
            ADD CONSTRAINT acq_request_tasks_kind_check CHECK (kind IN ('list', 'body', 'attachment', 'group'));

        CREATE TABLE acq_group_receipts (
            attempt_id bigint PRIMARY KEY REFERENCES acq_fetch_attempts,
            task_key text NOT NULL UNIQUE REFERENCES acq_request_tasks,
            list_attempt_id bigint NOT NULL REFERENCES acq_discovery_pages,
            run_id bigint NOT NULL REFERENCES acq_collection_runs,
            source_id text NOT NULL,
            group_key text NOT NULL CHECK (length(group_key) BETWEEN 1 AND 512),
            artifact_id bigint NOT NULL,
            channel text NOT NULL CHECK (channel <> ''),
            window_start date NOT NULL,
            window_end date NOT NULL CHECK (window_end >= window_start),
            received_at timestamptz NOT NULL,
            media_type text NOT NULL CHECK (length(media_type) BETWEEN 1 AND 128),
            size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 1 AND 20971520),
            members jsonb NOT NULL CHECK (jsonb_typeof(members) = 'array' AND jsonb_array_length(members) <= 256),
            CHECK (attempt_id <> list_attempt_id),
            UNIQUE (attempt_id, source_id, artifact_id, group_key),
            FOREIGN KEY (source_id, artifact_id) REFERENCES acq_raw_artifacts (source_id, id)
        );
        CREATE TRIGGER acq_group_receipts_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_group_receipts
            FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();

        ALTER TABLE acq_source_observations
            ADD COLUMN selector jsonb,
            ADD COLUMN record_digest text,
            ADD COLUMN group_key text GENERATED ALWAYS AS (selector->>'group_key') STORED,
            ADD CONSTRAINT acq_source_observations_selector CHECK (
                (selector IS NULL AND record_digest IS NULL) OR (
                    selector IS NOT NULL AND record_digest IS NOT NULL AND published_on IS NOT NULL
                    AND record_digest ~ '^sha256:[0-9a-f]{64}$'
                    AND (jsonb_typeof(selector) = 'object'
                         AND selector - ARRAY['kind', 'version', 'group_key', 'notice_key'] = '{}'::jsonb
                         AND jsonb_typeof(selector->'kind') = 'string'
                         AND selector->>'kind' ~ '^[a-z][a-z0-9_-]{0,95}$'
                         AND jsonb_typeof(selector->'version') = 'number'
                         AND selector->>'version' ~ '^[1-9][0-9]*$'
                         AND selector->'version' <= '2147483647'::jsonb
                         AND jsonb_typeof(selector->'group_key') = 'string'
                         AND jsonb_typeof(selector->'notice_key') = 'string'
                         AND selector->>'notice_key' = notice_key) IS TRUE
                )
            ),
            ADD FOREIGN KEY (attempt_id, source_id, artifact_id, group_key)
                REFERENCES acq_group_receipts (attempt_id, source_id, artifact_id, group_key);
        CREATE UNIQUE INDEX acq_source_observations_group_attempt_notice
            ON acq_source_observations (attempt_id, notice_key) WHERE selector IS NOT NULL;
    """)


def downgrade():
    op.execute("""
        LOCK TABLE acq_request_tasks, acq_group_receipts, acq_source_observations IN ACCESS EXCLUSIVE MODE;
        DO $$ BEGIN
            IF EXISTS (SELECT 1 FROM acq_group_receipts)
                OR EXISTS (SELECT 1 FROM acq_request_tasks WHERE kind = 'group') THEN
                RAISE EXCEPTION 'cannot downgrade with grouped acquisition history';
            END IF;
        END $$;
        DROP INDEX acq_source_observations_group_attempt_notice;
        ALTER TABLE acq_source_observations DROP COLUMN group_key, DROP COLUMN selector, DROP COLUMN record_digest;
        DROP TABLE acq_group_receipts;
        ALTER TABLE acq_request_tasks DROP CONSTRAINT acq_request_tasks_kind_check,
            ADD CONSTRAINT acq_request_tasks_kind_check CHECK (kind IN ('list', 'body', 'attachment'));
    """)
