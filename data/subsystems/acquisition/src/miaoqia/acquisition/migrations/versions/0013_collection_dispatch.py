"""Persist collection schedule cursors and window plans."""

from alembic import op

revision = "0013"
down_revision = "0012"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        CREATE TABLE acq_collection_schedule (
            source_id text PRIMARY KEY REFERENCES acq_sources(source_id),
            next_due_at timestamptz NOT NULL
        );

        CREATE UNIQUE INDEX acq_collection_runs_plan_target
            ON acq_collection_runs (id, source_id, mode);

        CREATE TABLE acq_collection_plans (
            id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
            source_id text NOT NULL REFERENCES acq_sources(source_id),
            mode text NOT NULL CHECK (mode IN ('incremental', 'backfill')),
            occurrence_key text NOT NULL CHECK (btrim(occurrence_key) <> ''),
            due_at timestamptz NOT NULL,
            not_before timestamptz NOT NULL CHECK (not_before >= due_at),
            window_start date NOT NULL,
            window_end date NOT NULL CHECK (window_end >= window_start),
            max_pages integer NOT NULL CHECK (max_pages > 0),
            refetch boolean NOT NULL,
            handler text NOT NULL CHECK (btrim(handler) <> ''),
            config_fingerprint text NOT NULL CHECK (config_fingerprint ~ '^[0-9a-f]{64}$'),
            budget_seconds integer NOT NULL CHECK (budget_seconds BETWEEN 1 AND 86400),
            run_id bigint UNIQUE,
            settled_at timestamptz CHECK (settled_at IS NULL OR run_id IS NOT NULL),
            UNIQUE (source_id, mode, occurrence_key),
            FOREIGN KEY (run_id, source_id, mode) REFERENCES acq_collection_runs (id, source_id, mode)
        );

        CREATE INDEX acq_collection_plans_ready
            ON acq_collection_plans (mode, not_before, due_at, id) WHERE settled_at IS NULL;
        CREATE INDEX acq_collection_plans_open_backfill_day
            ON acq_collection_plans (source_id, window_start) WHERE mode = 'backfill' AND settled_at IS NULL;
        CREATE INDEX acq_collection_plans_incremental_day
            ON acq_collection_plans (source_id, window_end, config_fingerprint, due_at)
            WHERE mode = 'incremental';
    """)


def downgrade():
    op.execute("""
        LOCK TABLE acq_collection_plans IN ACCESS EXCLUSIVE MODE;
        DO $$ BEGIN
            IF EXISTS (SELECT 1 FROM acq_collection_plans WHERE settled_at IS NULL) THEN
                RAISE EXCEPTION 'cannot drop dispatch state with unsettled collection plans';
            END IF;
        END $$;
        DROP TABLE acq_collection_plans, acq_collection_schedule;
        DROP INDEX acq_collection_runs_plan_target;
    """)
