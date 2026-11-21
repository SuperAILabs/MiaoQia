"""Bind rendered listing receipts to one navigation and network response."""
from alembic import op

revision = "0017"
down_revision = "0016"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        ALTER TABLE acq_discovery_pages ADD COLUMN browser_binding jsonb
            CHECK (browser_binding IS NULL OR jsonb_typeof(browser_binding) = 'object')
    """)


def downgrade():
    op.execute("""
        LOCK TABLE acq_discovery_pages IN ACCESS EXCLUSIVE MODE;
        DO $$ BEGIN
            IF EXISTS (SELECT 1 FROM acq_discovery_pages WHERE browser_binding IS NOT NULL) THEN
                RAISE EXCEPTION 'cannot downgrade with browser discovery history';
            END IF;
        END $$;
        ALTER TABLE acq_discovery_pages DROP COLUMN browser_binding;
    """)
