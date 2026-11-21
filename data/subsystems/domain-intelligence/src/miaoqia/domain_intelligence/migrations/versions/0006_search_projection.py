"""Domain owner revision ledger for the public search projection."""

from alembic import op

revision = "0006"
down_revision = "0005"
branch_labels = depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE dom_search_projection (
    fact_id text PRIMARY KEY REFERENCES dom_facts(id),
    public_version integer NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    digest text NOT NULL,
    source_ids text[] NOT NULL CHECK (cardinality(source_ids) > 0),
    FOREIGN KEY (fact_id, public_version) REFERENCES dom_fact_versions(fact_id, version)
);
CREATE INDEX dom_search_projection_sources ON dom_search_projection USING gin(source_ids);
CREATE INDEX dom_search_projection_closure_sources
 ON dom_version_requirements USING gin ((closure->'sources') jsonb_path_ops);
""")


def downgrade():
    op.execute("""
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM dom_search_projection) THEN
        RAISE EXCEPTION 'public projection revision history cannot be reset';
    END IF;
END $$;
DROP INDEX dom_search_projection_closure_sources;
DROP TABLE dom_search_projection;
""")
