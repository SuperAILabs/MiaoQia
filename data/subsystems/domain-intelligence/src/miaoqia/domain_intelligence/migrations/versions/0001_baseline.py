"""领域智能基线结构（domain-intelligence.md 第4节 fact_store）。

一条领域事实由信息领域、对象类型与来源对象（来源、来源对象键与对象在该来源对象中的序号）确定；同一来源对象的新快照
产生同一事实的新版本。版本不可变（触发器拒绝 UPDATE/DELETE），current_version 只能前进。
recorded_at 是平台记录该版本的时间（数据架构第5节：当时已知口径按它取版本），observed_at 来自快照。
另含受控派生版本、项目编号身份与公告关联、增强 receipt、固定版本输入闭包、快照进度与公共搜索投影修订账本。

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
    derivation_key text NOT NULL DEFAULT 'base',   -- 同一修订的受控派生版本；源版本为 base
    PRIMARY KEY (fact_id, version),
    CONSTRAINT dom_versions_derivation_unique UNIQUE (fact_id, revision_ref, derivation_key)
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

-- 编号身份与不可变公告关联/项目依赖。
CREATE TABLE dom_project_keys (
 namespace text NOT NULL, buyer_key text NOT NULL, project_code text NOT NULL,
 project_id text NOT NULL UNIQUE, rule_version text NOT NULL,
 PRIMARY KEY(namespace,buyer_key,project_code)
);
CREATE TABLE dom_notice_links (
 notice_id text NOT NULL, notice_version integer NOT NULL, project_id text,
 status text NOT NULL, reason jsonb NOT NULL,
 PRIMARY KEY(notice_id,notice_version),
 FOREIGN KEY(notice_id,notice_version) REFERENCES dom_fact_versions(fact_id,version)
);
CREATE TABLE dom_project_dependencies (
 project_id text NOT NULL, project_version integer NOT NULL,
 notice_id text NOT NULL, notice_version integer NOT NULL,
 PRIMARY KEY(project_id,project_version,notice_id),
 FOREIGN KEY(project_id,project_version) REFERENCES dom_fact_versions(fact_id,version),
 FOREIGN KEY(notice_id,notice_version) REFERENCES dom_fact_versions(fact_id,version)
);
CREATE TRIGGER dom_notice_links_immutable BEFORE UPDATE OR DELETE ON dom_notice_links
 FOR EACH ROW EXECUTE FUNCTION dom_versions_immutable();
CREATE TRIGGER dom_project_dependencies_immutable BEFORE UPDATE OR DELETE ON dom_project_dependencies
 FOR EACH ROW EXECUTE FUNCTION dom_versions_immutable();
CREATE TRIGGER dom_project_keys_immutable BEFORE UPDATE OR DELETE ON dom_project_keys
 FOR EACH ROW EXECUTE FUNCTION dom_versions_immutable();

-- 固定source-base/parent和可验证evidence结果的不可变应用receipt。
CREATE TABLE dom_enrichment_receipts (
 run_key text PRIMARY KEY, notice_id text NOT NULL, source_base_version integer NOT NULL,
 parent_version integer NOT NULL, output_version integer NOT NULL,
 recipe text NOT NULL, source_hash text NOT NULL, input_hash text NOT NULL, output_hash text NOT NULL,
 reference_hash text NOT NULL, evidence_refs jsonb NOT NULL, outputs jsonb NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(notice_id,source_base_version) REFERENCES dom_fact_versions(fact_id,version),
 FOREIGN KEY(notice_id,parent_version) REFERENCES dom_fact_versions(fact_id,version),
 FOREIGN KEY(notice_id,output_version) REFERENCES dom_fact_versions(fact_id,version)
);
CREATE TRIGGER dom_enrichment_receipts_immutable BEFORE UPDATE OR DELETE ON dom_enrichment_receipts
 FOR EACH ROW EXECUTE FUNCTION dom_versions_immutable();

-- Immutable fixed-version input closure; permission remains current.
CREATE TABLE dom_version_requirements (
 fact_id text NOT NULL, version integer NOT NULL, subject_hash text NOT NULL,
 closure jsonb NOT NULL, proof_hash text NOT NULL, recorded_at timestamptz NOT NULL DEFAULT now(),
 actor text NOT NULL, reason text NOT NULL, db_actor text NOT NULL DEFAULT session_user,
 PRIMARY KEY(fact_id,version), FOREIGN KEY(fact_id,version) REFERENCES dom_fact_versions(fact_id,version));
CREATE FUNCTION dom_requirement_audit() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.actor !~ '^[a-zA-Z0-9_-]{1,96}$' OR length(NEW.reason) NOT BETWEEN 1 AND 256 OR NEW.reason ~ '[[:cntrl:]]' THEN
 RAISE EXCEPTION 'invalid audit metadata'; END IF;
 NEW.db_actor=session_user; NEW.recorded_at=clock_timestamp(); RETURN NEW;
END $$;
CREATE TRIGGER dom_requirements_audit BEFORE INSERT ON dom_version_requirements
 FOR EACH ROW EXECUTE FUNCTION dom_requirement_audit();
CREATE TRIGGER dom_requirements_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON dom_version_requirements
 FOR EACH STATEMENT EXECUTE FUNCTION dom_versions_immutable();

-- The latest accepted evidence snapshot, tracked even when its facts are unchanged.
CREATE TABLE dom_snapshot_progress (
    info_domain text NOT NULL,
    source_id text NOT NULL,
    source_key text NOT NULL,
    snapshot_seq bigint NOT NULL CHECK (snapshot_seq > 0),
    PRIMARY KEY (info_domain, source_id, source_key)
);

-- Domain owner revision ledger for the public search projection, including every search chunk slot announced
-- for a notice so that tombstones can be replayed.
CREATE TABLE dom_search_projection (
    fact_id text PRIMARY KEY REFERENCES dom_facts(id),
    public_version integer NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    digest text NOT NULL,
    source_ids text[] NOT NULL CHECK (cardinality(source_ids) > 0),
    chunk_slots smallint NOT NULL DEFAULT 0 CHECK (chunk_slots BETWEEN 0 AND 12),
    FOREIGN KEY (fact_id, public_version) REFERENCES dom_fact_versions(fact_id, version)
);
CREATE INDEX dom_search_projection_sources ON dom_search_projection USING gin(source_ids);
CREATE INDEX dom_search_projection_closure_sources
 ON dom_version_requirements USING gin ((closure->'sources') jsonb_path_ops);
""")


def downgrade() -> None:
    raise RuntimeError("domain fact history cannot be destructively downgraded")
