"""Retain the immutable group selection for each accepted observation."""
from alembic import op

revision = "0007"
down_revision = "0006"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        CREATE TABLE evd_grouped_observations (
            source_id text NOT NULL,
            notice_key text NOT NULL,
            observation_version bigint NOT NULL CHECK (observation_version > 0),
            snapshot_id text NOT NULL REFERENCES evd_snapshots (id),
            selector jsonb NOT NULL CHECK (
                (jsonb_typeof(selector) = 'object'
                    AND selector - ARRAY['kind', 'version', 'group_key', 'notice_key'] = '{}'::jsonb
                    AND jsonb_typeof(selector->'kind') = 'string'
                    AND selector->>'kind' ~ '^[a-z][a-z0-9_-]{0,95}$'
                    AND jsonb_typeof(selector->'version') = 'number'
                    AND selector->>'version' ~ '^[1-9][0-9]*$'
                    AND selector->'version' <= '2147483647'::jsonb
                    AND jsonb_typeof(selector->'group_key') = 'string'
                    AND length(selector->>'group_key') BETWEEN 1 AND 512
                    AND jsonb_typeof(selector->'notice_key') = 'string'
                    AND selector->>'notice_key' = notice_key) IS TRUE
            ),
            published_on date NOT NULL,
            record_digest text NOT NULL CHECK (record_digest ~ '^sha256:[0-9a-f]{64}$'),
            raw_sha256 text NOT NULL CHECK (raw_sha256 ~ '^sha256:[0-9a-f]{64}$'),
            media_type text NOT NULL CHECK (length(media_type) BETWEEN 1 AND 128),
            size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 1 AND 20971520),
            is_anchor boolean NOT NULL,
            PRIMARY KEY (source_id, notice_key, observation_version),
            FOREIGN KEY (source_id, notice_key) REFERENCES evd_live_observations (source_id, notice_key)
        );
        CREATE UNIQUE INDEX evd_grouped_observations_anchor
            ON evd_grouped_observations (snapshot_id) WHERE is_anchor;
        CREATE TRIGGER evd_grouped_observations_immutable BEFORE UPDATE OR DELETE ON evd_grouped_observations
            FOR EACH ROW EXECUTE FUNCTION evd_immutable();
        CREATE TRIGGER evd_grouped_observations_no_truncate BEFORE TRUNCATE ON evd_grouped_observations
            FOR EACH STATEMENT EXECUTE FUNCTION evd_immutable();
    """)


def downgrade():
    op.execute("""
        LOCK TABLE evd_grouped_observations, evd_pending_live_events IN ACCESS EXCLUSIVE MODE;
        DO $$ BEGIN
            IF EXISTS (SELECT 1 FROM evd_grouped_observations)
                OR EXISTS (SELECT 1 FROM evd_pending_live_events
                           WHERE envelope->>'type' = 'acquisition.fetch.completed.v3') THEN
                RAISE EXCEPTION 'cannot downgrade with grouped evidence observations';
            END IF;
        END $$;
        DROP TABLE evd_grouped_observations;
    """)
