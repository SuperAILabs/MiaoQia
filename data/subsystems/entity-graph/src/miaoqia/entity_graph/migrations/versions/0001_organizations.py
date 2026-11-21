"""Append-only public organization identity and version authority."""
from alembic import op

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE ent_organizations (
    org_id text PRIMARY KEY CHECK (org_id ~ '^org_[0-9A-HJKMNP-TV-Z]{26}$'),
    identifier_namespace text NOT NULL,
    identifier_value text NOT NULL,
    fact_id text NOT NULL UNIQUE CHECK (fact_id ~ '^ogr_[0-9A-HJKMNP-TV-Z]{26}$'),
    source_id text NOT NULL,
    source_key text NOT NULL,
    UNIQUE (identifier_namespace, identifier_value)
);
CREATE TABLE ent_organization_versions (
    org_id text NOT NULL REFERENCES ent_organizations (org_id),
    version bigint NOT NULL CHECK (version > 0),
    fact_version bigint NOT NULL CHECK (fact_version > 0),
    recorded_at timestamptz NOT NULL,
    name text NOT NULL CHECK (name <> ''),
    revision_ref text NOT NULL CHECK (revision_ref ~ '^exr_[0-9A-HJKMNP-TV-Z]{26}$'),
    snapshot_ref text NOT NULL CHECK (snapshot_ref ~ '^snp_[0-9A-HJKMNP-TV-Z]{26}$'),
    raw_sha256 text NOT NULL CHECK (raw_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    name_evidence_ref text NOT NULL CHECK (name_evidence_ref ~ '^ev_[0-9A-HJKMNP-TV-Z]{26}$'),
    identifier_path text NOT NULL,
    identifier_evidence_ref text NOT NULL CHECK (identifier_evidence_ref ~ '^ev_[0-9A-HJKMNP-TV-Z]{26}$'),
    PRIMARY KEY (org_id, version),
    UNIQUE (org_id, fact_version),
    CHECK (name_evidence_ref <> identifier_evidence_ref)
);
CREATE INDEX ent_organization_versions_name ON ent_organization_versions (name, org_id);
CREATE INDEX ent_organization_versions_known_at ON ent_organization_versions (org_id, recorded_at DESC);

CREATE TABLE ent_organization_conflicts (
    org_id text NOT NULL REFERENCES ent_organizations (org_id),
    contender_fact_id text NOT NULL CHECK (contender_fact_id ~ '^ogr_[0-9A-HJKMNP-TV-Z]{26}$'),
    contender_fact_version bigint NOT NULL CHECK (contender_fact_version > 0),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, contender_fact_id, contender_fact_version)
);
CREATE FUNCTION ent_organizations_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'organization identities and versions are append-only' USING ERRCODE = '55000';
END
$$;
CREATE TRIGGER ent_organizations_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON ent_organizations
    FOR EACH STATEMENT EXECUTE FUNCTION ent_organizations_immutable();
CREATE TRIGGER ent_organization_versions_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON ent_organization_versions
    FOR EACH STATEMENT EXECUTE FUNCTION ent_organizations_immutable();
CREATE TRIGGER ent_organization_conflicts_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON ent_organization_conflicts
    FOR EACH STATEMENT EXECUTE FUNCTION ent_organizations_immutable();
""")


def downgrade():
    op.execute("""
DROP TABLE ent_organization_conflicts, ent_organization_versions, ent_organizations;
DROP FUNCTION ent_organizations_immutable();
""")
