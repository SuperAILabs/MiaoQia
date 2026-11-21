"""Track all search chunk slots for each announced notice and replay their tombstones."""
from alembic import op

revision = "0007"
down_revision = "0006"
branch_labels = depends_on = None


def upgrade():
    op.execute("""
        ALTER TABLE dom_search_projection ADD COLUMN chunk_slots smallint NOT NULL DEFAULT 0
            CHECK (chunk_slots BETWEEN 0 AND 12)
    """)


def downgrade():
    op.execute("""
        DO $$ BEGIN
            IF EXISTS (SELECT 1 FROM dom_search_projection WHERE chunk_slots > 0) THEN
                RAISE EXCEPTION 'public chunk tombstone history cannot be reset';
            END IF;
        END $$;
        ALTER TABLE dom_search_projection DROP COLUMN chunk_slots
    """)
