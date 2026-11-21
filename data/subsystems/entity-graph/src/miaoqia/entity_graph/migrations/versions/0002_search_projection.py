"""Durable organization discovery revisions and bounded source pagination."""

from alembic import op

revision = "0002"
down_revision = "0001"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
        CREATE TABLE ent_search_projection (
            org_id text PRIMARY KEY,
            public_version bigint NOT NULL CHECK (public_version > 0),
            revision bigint NOT NULL CHECK (revision > 0),
            digest text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
            FOREIGN KEY (org_id, public_version) REFERENCES ent_organization_versions (org_id, version)
        );
        CREATE INDEX ent_organizations_source_order ON ent_organizations (source_id, org_id COLLATE "C");
    """)


def downgrade():
    op.execute("DROP INDEX ent_organizations_source_order; DROP TABLE ent_search_projection;")
