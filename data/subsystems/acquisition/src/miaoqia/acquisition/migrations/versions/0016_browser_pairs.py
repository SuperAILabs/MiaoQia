"""Bind browser receipts to observations while keeping original and projection roles distinct."""
from alembic import op

revision = "0016"
down_revision = "0015"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        ALTER TABLE acq_request_tasks DROP CONSTRAINT acq_request_tasks_kind_check,
            ADD CONSTRAINT acq_request_tasks_kind_check
                CHECK (kind IN ('list', 'body', 'attachment', 'group', 'browser'));

        CREATE TABLE acq_browser_receipts (
            attempt_id bigint PRIMARY KEY REFERENCES acq_fetch_attempts,
            task_key text NOT NULL UNIQUE REFERENCES acq_request_tasks,
            list_attempt_id bigint NOT NULL REFERENCES acq_discovery_pages,
            run_id bigint NOT NULL REFERENCES acq_collection_runs,
            source_id text NOT NULL,
            notice_key text NOT NULL CHECK (length(notice_key) BETWEEN 1 AND 512),
            artifact_id bigint NOT NULL,
            payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
            CHECK (attempt_id <> list_attempt_id),
            UNIQUE (attempt_id, source_id, notice_key, artifact_id),
            FOREIGN KEY (source_id, artifact_id) REFERENCES acq_raw_artifacts (source_id, id)
        );
        CREATE TRIGGER acq_browser_receipts_append_only
            BEFORE UPDATE OR DELETE OR TRUNCATE ON acq_browser_receipts
            FOR EACH STATEMENT EXECUTE FUNCTION acq_append_only();

        ALTER TABLE acq_source_observations
            ADD COLUMN browser_attempt_id bigint UNIQUE,
            ADD CONSTRAINT acq_source_observations_browser CHECK (
                browser_attempt_id IS NULL OR (
                    browser_attempt_id = attempt_id AND selector IS NULL AND published_on IS NOT NULL
                )
            ),
            ADD FOREIGN KEY (browser_attempt_id, source_id, notice_key, artifact_id)
                REFERENCES acq_browser_receipts (attempt_id, source_id, notice_key, artifact_id);
    """)


def downgrade():
    op.execute("""
        LOCK TABLE acq_request_tasks, acq_browser_receipts, acq_source_observations IN ACCESS EXCLUSIVE MODE;
        DO $$ BEGIN
            IF EXISTS (SELECT 1 FROM acq_browser_receipts)
                OR EXISTS (SELECT 1 FROM acq_request_tasks WHERE kind = 'browser') THEN
                RAISE EXCEPTION 'cannot downgrade with browser acquisition history';
            END IF;
        END $$;
        ALTER TABLE acq_source_observations DROP COLUMN browser_attempt_id;
        DROP TABLE acq_browser_receipts;
        ALTER TABLE acq_request_tasks DROP CONSTRAINT acq_request_tasks_kind_check,
            ADD CONSTRAINT acq_request_tasks_kind_check CHECK (kind IN ('list', 'body', 'attachment', 'group'));
    """)
