"""公共实例的快照、解析修订与文档证据（evidence.md 第4节）。

正文与原始制品存对象存储，数据库只存元数据、对象键、解析结果（结构化字段与定位符）与证据定位。
快照、解析修订与证据引用不可变：应用账号的 UPDATE/DELETE 由触发器拒绝，修正只能追加新的修订。

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
-- 来源快照：同一来源对象（source_id + source_key）的规范化内容与最新快照不同即为新快照，seq 自 1 递增；
-- 内容回到历史取值（A→B→A）同样是来源的一次变化，记为新快照，不复用历史快照。
CREATE TABLE evd_snapshots (
    id               text PRIMARY KEY CHECK (id ~ '^snp_[0-9A-HJKMNP-TV-Z]{26}$'),
    source_id        text NOT NULL CHECK (source_id <> ''),
    source_key       text NOT NULL CHECK (source_key <> ''),
    seq              integer NOT NULL CHECK (seq >= 1),
    normalized_hash  text NOT NULL CHECK (normalized_hash ~ '^sha256:[0-9a-f]{64}$'),
    raw_sha256       text NOT NULL CHECK (raw_sha256 ~ '^sha256:[0-9a-f]{64}$'),
    artifact_ref     text NOT NULL,          -- 原始制品对象键（raw/…），不是签名地址
    document_ref     text NOT NULL,          -- 规范化文档对象键（normalized/…）
    media_type       text NOT NULL,
    pipeline_version text NOT NULL,
    rights_tags      jsonb NOT NULL CHECK (jsonb_typeof(rights_tags) = 'array' AND jsonb_array_length(rights_tags) >= 1),
    observed_at      timestamptz NOT NULL,   -- 来源观察时间（采集或回放材料的取得时间）
    recorded_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source_id, source_key, seq)
);

-- 解析修订：一个快照在一个解析器版本下只有一个修订；人工修正与重放追加新修订。
CREATE TABLE evd_extraction_revisions (
    id             text PRIMARY KEY CHECK (id ~ '^exr_[0-9A-HJKMNP-TV-Z]{26}$'),
    snapshot_id    text NOT NULL REFERENCES evd_snapshots (id),
    extractor      text NOT NULL,           -- 解析器标识@版本
    pack_id        text NOT NULL CHECK (pack_id ~ '^INF-[A-Z]+$'),
    pack_version   text NOT NULL CHECK (pack_version ~ '^[0-9]+\\.[0-9]+\\.[0-9]+$'),
    facts          jsonb NOT NULL CHECK (jsonb_typeof(facts) = 'array'),
    quality_status text NOT NULL CHECK (quality_status IN ('ok', 'warning', 'suspect')),
    warnings       jsonb NOT NULL CHECK (jsonb_typeof(warnings) = 'array'),
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (snapshot_id, extractor)
);

-- 文档证据引用：快照、管线版本与定位器；quote_hash 用于复核原文未变。
CREATE TABLE evd_evidence_refs (
    id               text PRIMARY KEY CHECK (id ~ '^ev_[0-9A-HJKMNP-TV-Z]{26}$'),
    snapshot_id      text NOT NULL REFERENCES evd_snapshots (id),
    pipeline_version text NOT NULL,
    locator          jsonb NOT NULL CHECK (jsonb_typeof(locator) = 'object'),
    quote_hash       text NOT NULL CHECK (quote_hash ~ '^sha256:[0-9a-f]{64}$')
);
CREATE INDEX evd_evidence_refs_snapshot ON evd_evidence_refs (snapshot_id);

CREATE FUNCTION evd_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'evidence 记录不可变：%', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END $$;
CREATE TRIGGER evd_snapshots_immutable BEFORE UPDATE OR DELETE ON evd_snapshots
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_extraction_revisions_immutable BEFORE UPDATE OR DELETE ON evd_extraction_revisions
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();
CREATE TRIGGER evd_evidence_refs_immutable BEFORE UPDATE OR DELETE ON evd_evidence_refs
    FOR EACH ROW EXECUTE FUNCTION evd_immutable();
""")


def downgrade() -> None:
    op.execute("""
DROP TABLE evd_evidence_refs;
DROP TABLE evd_extraction_revisions;
DROP TABLE evd_snapshots;
DROP FUNCTION evd_immutable();
""")
