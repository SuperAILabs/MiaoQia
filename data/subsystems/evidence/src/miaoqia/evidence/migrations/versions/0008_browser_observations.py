"""Pin each accepted browser pair and its immutable snapshot anchor."""
from alembic import op

revision = "0008"
down_revision = "0007"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        CREATE TABLE evd_browser_observations (
            source_id text NOT NULL,
            notice_key text NOT NULL,
            observation_version bigint NOT NULL CHECK (observation_version > 0),
            snapshot_id text NOT NULL REFERENCES evd_snapshots (id),
            event jsonb NOT NULL CHECK (
                (jsonb_typeof(event) = 'object'
                    AND event->'source_id' = to_jsonb(source_id)
                    AND event->'notice_key' = to_jsonb(notice_key)
                    AND event->'observation_version' = to_jsonb(observation_version)
                    AND event->>'binding_digest' ~ '^sha256:[0-9a-f]{64}$') IS TRUE
            ),
            is_anchor boolean NOT NULL,
            PRIMARY KEY (source_id, notice_key, observation_version),
            FOREIGN KEY (source_id, notice_key) REFERENCES evd_live_observations (source_id, notice_key)
        );
        CREATE UNIQUE INDEX evd_browser_observations_anchor
            ON evd_browser_observations (snapshot_id) WHERE is_anchor;
        CREATE INDEX evd_browser_observations_url ON evd_browser_observations (source_id, (event->>'source_url'));
        CREATE TRIGGER evd_browser_observations_immutable BEFORE UPDATE OR DELETE ON evd_browser_observations
            FOR EACH ROW EXECUTE FUNCTION evd_immutable();
        CREATE TRIGGER evd_browser_observations_no_truncate BEFORE TRUNCATE ON evd_browser_observations
            FOR EACH STATEMENT EXECUTE FUNCTION evd_immutable();
        CREATE UNIQUE INDEX evd_pending_browser_version ON evd_pending_live_events
            (source_id, (envelope #>> '{data,notice_key}'), (envelope #>> '{data,observation_version}'))
            WHERE envelope->>'type' = 'acquisition.fetch.completed.v4';
    """)


def downgrade():
    op.execute("""
        LOCK TABLE evd_browser_observations, evd_pending_live_events IN ACCESS EXCLUSIVE MODE;
        DO $$ BEGIN
            IF EXISTS (SELECT 1 FROM evd_browser_observations)
                OR EXISTS (SELECT 1 FROM evd_pending_live_events
                           WHERE envelope->>'type' = 'acquisition.fetch.completed.v4') THEN
                RAISE EXCEPTION 'cannot downgrade with browser evidence observations';
            END IF;
        END $$;
        DROP INDEX evd_pending_browser_version;
        DROP TABLE evd_browser_observations;
    """)
