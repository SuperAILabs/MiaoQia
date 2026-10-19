-- +goose Up
-- 客户档案、上下文版本与快照、目标配置（PRD 第2段）。全部为租户私有数据：行级安全按事务会话变量 app.tenant_id 过滤
-- （kernel/db.Tx 设置），应用账号 NOBYPASSRLS。档案版本与快照不可变：应用账号只能写入与查询。

-- 租户上下文版本：档案（以及后续的清单与单元覆盖）的每次提交在同一事务中递增 context_version。
-- refresh_after 是当前快照中资质状态可能因日期变化的最早日期，到达后首次读取时形成新版本。
CREATE TABLE ctx_contexts (
    tenant_id       text PRIMARY KEY CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    context_version bigint NOT NULL CHECK (context_version >= 1),
    profile_version bigint NOT NULL CHECK (profile_version >= 1),
    refresh_after   date,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- 档案版本（ProfileVersion）：每次保存一个版本，内容为整份档案。
CREATE TABLE ctx_profile_versions (
    tenant_id       text NOT NULL REFERENCES ctx_contexts(tenant_id),
    version         bigint NOT NULL CHECK (version >= 1),
    context_version bigint NOT NULL CHECK (context_version >= 1),
    content         jsonb NOT NULL,
    created_by      text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, version)
);

-- 快照：（租户、组织单元、上下文版本）唯一确定一个合成结果，写入后不变。
CREATE TABLE ctx_snapshots (
    tenant_id       text NOT NULL REFERENCES ctx_contexts(tenant_id),
    org_unit_id     text NOT NULL,
    context_version bigint NOT NULL CHECK (context_version >= 1),
    profile_version bigint NOT NULL CHECK (profile_version >= 1),
    unit_chain      text[] NOT NULL CHECK (cardinality(unit_chain) >= 1),
    as_of           date NOT NULL,
    policy_view     jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, org_unit_id, context_version),
    FOREIGN KEY (tenant_id, profile_version) REFERENCES ctx_profile_versions(tenant_id, version)
);

-- 目标（TenantObjective）：同一单元同一模板只启用一次。
CREATE TABLE ctx_objectives (
    id                      text PRIMARY KEY CHECK (id ~ '^obj_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id               text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    org_unit_id             text NOT NULL,
    template_id             text NOT NULL,
    template_version        text NOT NULL,
    policy_template_id      text NOT NULL,
    policy_template_version text NOT NULL,
    state                   text NOT NULL CHECK (state IN ('inactive', 'active', 'disabled')),
    owner                   text NOT NULL,
    participants            text[] NOT NULL,
    params                  jsonb NOT NULL,
    version                 bigint NOT NULL CHECK (version >= 1),
    created_by              text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, org_unit_id, template_id)
);

-- 命令幂等：同一主体、租户与 Idempotency-Key 返回首次结果；键相同而请求不同拒绝。与业务写入、发件箱、审计同事务。
CREATE TABLE ctx_commands (
    tenant_id   text NOT NULL,
    actor       text NOT NULL,
    request_key text NOT NULL CHECK (request_key ~ '^[A-Za-z0-9_-]{16,128}$'),
    operation   text NOT NULL,
    fingerprint text NOT NULL,
    response    jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, actor, request_key)
);

ALTER TABLE ctx_contexts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_profile_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_objectives ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_commands ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ctx_contexts USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_profile_versions USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_snapshots USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_objectives USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_commands USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON ctx_profile_versions, ctx_snapshots FROM ${MIAOQIA_DB_APP_ROLE};
REVOKE DELETE, TRUNCATE ON ctx_contexts, ctx_objectives, ctx_commands FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE ctx_commands;
DROP TABLE ctx_objectives;
DROP TABLE ctx_snapshots;
DROP TABLE ctx_profile_versions;
DROP TABLE ctx_contexts;
