"""受控派生版本、编号身份与不可变公告关联/项目依赖。"""
from alembic import op
revision = "0002"
down_revision = "0001"
branch_labels = None
depends_on = None

def upgrade():
    op.execute("""
ALTER TABLE dom_fact_versions ADD COLUMN derivation_key text NOT NULL DEFAULT 'base';
ALTER TABLE dom_fact_versions DROP CONSTRAINT dom_fact_versions_fact_id_revision_ref_key;
ALTER TABLE dom_fact_versions ADD CONSTRAINT dom_versions_derivation_unique UNIQUE(fact_id, revision_ref, derivation_key);
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
""")

def downgrade():
    # 回退不得丢失关联依据或把已合法的多推导版本强行压回旧唯一约束。
    op.execute("""DO $$ BEGIN
      IF EXISTS (SELECT 1 FROM dom_notice_links)
         OR EXISTS (SELECT 1 FROM dom_fact_versions GROUP BY fact_id,revision_ref HAVING count(*)>1) THEN
        RAISE EXCEPTION '聚合/关联数据已存在，拒绝有损回退；请保留数据并前向迁移';
      END IF;
    END $$;""")
    op.execute("DROP TABLE dom_project_dependencies; DROP TABLE dom_notice_links; DROP TABLE dom_project_keys;")
    op.execute("ALTER TABLE dom_fact_versions DROP CONSTRAINT dom_versions_derivation_unique;")
    op.execute("ALTER TABLE dom_fact_versions DROP COLUMN derivation_key; ALTER TABLE dom_fact_versions ADD UNIQUE(fact_id,revision_ref);")
