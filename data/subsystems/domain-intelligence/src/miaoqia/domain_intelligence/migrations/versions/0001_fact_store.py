"""领域事实与版本链（domain-intelligence.md 第4节 fact_store）。

一条领域事实由信息领域、对象类型与来源对象（来源、来源对象键与对象在该来源对象中的序号）确定；同一来源对象的新快照
产生同一事实的新版本。版本不可变（触发器拒绝 UPDATE/DELETE），current_version 只能前进。
recorded_at 是平台记录该版本的时间（数据架构第5节：当时已知口径按它取版本），observed_at 来自快照。

Revision ID: 0001
Revises:
"""
from alembic import op

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.execute("""
CREATE TABLE dom_facts (
    id              text PRIMARY KEY CHECK (id ~ '^[a-z]{2,4}_[0-9A-HJKMNP-TV-Z]{26}$'),
    info_domain     text NOT NULL CHECK (info_domain ~ '^INF-[A-Z]+$'),
    object_type     text NOT NULL,
    source_id       text NOT NULL,
    source_key      text NOT NULL,
    ordinal         integer NOT NULL CHECK (ordinal >= 0),
    current_version integer NOT NULL CHECK (current_version >= 1),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (info_domain, object_type, source_id, source_key, ordinal)
);

CREATE TABLE dom_fact_versions (
    fact_id        text NOT NULL REFERENCES dom_facts (id),
    version        integer NOT NULL CHECK (version >= 1),
    content        jsonb NOT NULL,                 -- 完整领域事实（含公共字段与字段溯源）
    value_hash     text NOT NULL CHECK (value_hash ~ '^sha256:[0-9a-f]{64}$'),  -- 去掉溯源与部分标识后的取值哈希
    schema_version text NOT NULL,
    snapshot_ref   text NOT NULL CHECK (snapshot_ref ~ '^snp_[0-9A-HJKMNP-TV-Z]{26}$'),
    revision_ref   text NOT NULL CHECK (revision_ref ~ '^exr_[0-9A-HJKMNP-TV-Z]{26}$'),
    changed_fields jsonb NOT NULL CHECK (jsonb_typeof(changed_fields) = 'array' AND jsonb_array_length(changed_fields) >= 1),
    rights_tags    jsonb NOT NULL CHECK (jsonb_typeof(rights_tags) = 'array' AND jsonb_array_length(rights_tags) >= 1),
    observed_at    timestamptz NOT NULL,
    recorded_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (fact_id, version),
    UNIQUE (fact_id, revision_ref)
);

CREATE FUNCTION dom_versions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '领域事实版本不可变' USING ERRCODE = 'integrity_constraint_violation';
END $$;
CREATE TRIGGER dom_fact_versions_immutable BEFORE UPDATE OR DELETE ON dom_fact_versions
    FOR EACH ROW EXECUTE FUNCTION dom_versions_immutable();

CREATE FUNCTION dom_current_forward() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.current_version <= OLD.current_version OR NEW.id <> OLD.id OR NEW.source_key <> OLD.source_key
       OR NEW.source_id <> OLD.source_id OR NEW.ordinal <> OLD.ordinal OR NEW.object_type <> OLD.object_type
       OR NEW.info_domain <> OLD.info_domain THEN
        RAISE EXCEPTION '领域事实当前版本只能前进，身份不可变' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER dom_facts_forward BEFORE UPDATE ON dom_facts FOR EACH ROW EXECUTE FUNCTION dom_current_forward();
CREATE TRIGGER dom_facts_no_delete BEFORE DELETE ON dom_facts FOR EACH ROW EXECUTE FUNCTION dom_versions_immutable();
""")


def downgrade() -> None:
    op.execute("""
DROP TABLE dom_fact_versions;
DROP TABLE dom_facts;
DROP FUNCTION dom_versions_immutable();
DROP FUNCTION dom_current_forward();
""")
