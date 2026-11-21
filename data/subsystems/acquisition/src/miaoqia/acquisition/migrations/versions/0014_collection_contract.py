"""Keep superseded plans and pin dispatch to the planner's active source contract."""

from alembic import op

revision = "0014"
down_revision = "0013"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        ALTER TABLE acq_sources ADD COLUMN collection_fingerprint text
            CHECK (collection_fingerprint ~ '^[0-9a-f]{64}$');

        ALTER TABLE acq_collection_plans
            DROP CONSTRAINT acq_collection_plans_source_id_mode_occurrence_key_key,
            DROP CONSTRAINT acq_collection_plans_check2,
            ADD COLUMN settlement_reason text,
            ADD CONSTRAINT acq_collection_plans_settlement CHECK (
                (settled_at IS NULL OR run_id IS NOT NULL OR settlement_reason IS NOT NULL)
                AND (settlement_reason IS NULL OR (settlement_reason = 'source_contract_changed'
                                                    AND settled_at IS NOT NULL))
            );
        CREATE UNIQUE INDEX acq_collection_plans_occurrence ON acq_collection_plans
            (source_id, mode, occurrence_key) WHERE settlement_reason IS NULL;
    """)


def downgrade():
    op.execute("""
        LOCK TABLE acq_collection_plans IN ACCESS EXCLUSIVE MODE;
        DO $$ BEGIN
            IF EXISTS (SELECT 1 FROM acq_collection_plans WHERE settlement_reason IS NOT NULL) THEN
                RAISE EXCEPTION 'cannot downgrade with superseded collection plans';
            END IF;
        END $$;
        DROP INDEX acq_collection_plans_occurrence;
        ALTER TABLE acq_collection_plans
            DROP CONSTRAINT acq_collection_plans_settlement,
            DROP COLUMN settlement_reason,
            ADD CHECK (settled_at IS NULL OR run_id IS NOT NULL),
            ADD UNIQUE (source_id, mode, occurrence_key);
        ALTER TABLE acq_sources DROP COLUMN collection_fingerprint;
    """)
