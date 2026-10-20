-- +goose Up
-- 策略草案、版本、仿真附着与激活/回滚申请（PRD 第7段；customer-context.md 第4、6节）。全部为租户私有数据，行级安全同 00001。
-- 每个目标一行 ctx_policies：策略代次（每次激活或回滚加一）、生效版本、草案指针与草案的乐观并发版本号。
-- 草案修订与策略版本的内容不可变（修订只追加；版本只改变状态 active / superseded）。

CREATE TABLE ctx_policies (
    objective_id      text PRIMARY KEY REFERENCES ctx_objectives(id),
    tenant_id         text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    generation        bigint NOT NULL DEFAULT 0 CHECK (generation >= 0),
    active_version    bigint NOT NULL DEFAULT 0 CHECK (active_version >= 0),
    latest_version    bigint NOT NULL DEFAULT 0 CHECK (latest_version >= active_version),
    latest_revision   bigint NOT NULL DEFAULT 0 CHECK (latest_revision >= 0),
    -- 当前草案修订（NULL 为没有草案）与草案的乐观并发版本号（每次保存或放弃加一，单调）
    draft_revision    bigint CHECK (draft_revision IS NULL OR draft_revision BETWEEN 1 AND latest_revision),
    draft_version     bigint NOT NULL DEFAULT 0 CHECK (draft_version >= 0),
    -- 锁定草案的进行中激活申请（审批请求标识）
    draft_locked_by   text CHECK (draft_locked_by IS NULL OR draft_revision IS NOT NULL),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ctx_policy_revisions (
    tenant_id          text NOT NULL,
    objective_id       text NOT NULL REFERENCES ctx_policies(objective_id),
    revision           bigint NOT NULL CHECK (revision >= 1),
    base_version       bigint NOT NULL CHECK (base_version >= 0),
    template_id        text NOT NULL,
    template_version   text NOT NULL,
    form               jsonb NOT NULL,
    -- 逐条规则（含停用的规则；合成文档只含启用的规则），版本历史与差异不依赖当前模板
    rules              jsonb NOT NULL,
    document           jsonb NOT NULL,
    valid              boolean NOT NULL,
    issues             jsonb NOT NULL,
    example_mismatches jsonb NOT NULL,
    saved_by           text NOT NULL,
    -- 业务时间
    saved_at           timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (objective_id, revision)
);

-- learning.simulation.completed.v1 附着到草案修订的仿真运行（报告本身在 learning）
CREATE TABLE ctx_policy_simulations (
    simulation_id      text PRIMARY KEY CHECK (simulation_id ~ '^sim_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id          text NOT NULL,
    objective_id       text NOT NULL REFERENCES ctx_policies(objective_id),
    revision           bigint NOT NULL CHECK (revision >= 1),
    status             text NOT NULL CHECK (status IN ('completed','failed')),
    failure_reason     text,
    compare_version    bigint NOT NULL CHECK (compare_version >= 0),
    compare_generation bigint NOT NULL CHECK (compare_generation >= 0),
    enabled_domains    text[] NOT NULL,
    completed_at       timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (objective_id, revision) REFERENCES ctx_policy_revisions(objective_id, revision)
);
CREATE INDEX ctx_policy_simulations_revision ON ctx_policy_simulations(objective_id, revision, completed_at DESC);

CREATE TABLE ctx_policy_versions (
    tenant_id        text NOT NULL,
    objective_id     text NOT NULL REFERENCES ctx_policies(objective_id),
    version          bigint NOT NULL CHECK (version >= 1),
    state            text NOT NULL CHECK (state IN ('active','superseded')),
    source_revision  bigint NOT NULL,
    template_id      text NOT NULL,
    template_version text NOT NULL,
    form             jsonb NOT NULL,
    rules            jsonb NOT NULL,
    document         jsonb NOT NULL,
    enabled_domains  text[] NOT NULL,
    simulation_id    text NOT NULL REFERENCES ctx_policy_simulations(simulation_id),
    approval_id      text NOT NULL,
    requested_by     text NOT NULL,
    approved_by      text NOT NULL CHECK (approved_by <> requested_by),
    -- 业务时间：首次激活
    activated_at     timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (objective_id, version),
    FOREIGN KEY (objective_id, source_revision) REFERENCES ctx_policy_revisions(objective_id, revision)
);
CREATE UNIQUE INDEX ctx_policy_versions_active ON ctx_policy_versions(objective_id) WHERE state = 'active';

-- 激活与回滚申请的本地执行记录：同一目标同时只有一个待审批申请
CREATE TABLE ctx_policy_requests (
    approval_id         text PRIMARY KEY CHECK (approval_id ~ '^apr_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id           text NOT NULL,
    objective_id        text NOT NULL REFERENCES ctx_policies(objective_id),
    kind                text NOT NULL CHECK (kind IN ('activation','rollback')),
    state               text NOT NULL CHECK (state IN ('pending','executed','failed','rejected','withdrawn','expired','invalidated')),
    object_version      bigint NOT NULL CHECK (object_version >= 1),
    simulation_id       text NOT NULL,
    baseline_version    bigint NOT NULL CHECK (baseline_version >= 0),
    baseline_generation bigint NOT NULL CHECK (baseline_generation >= 0),
    backfill_days       integer CHECK (backfill_days BETWEEN 0 AND 30),
    requested_by        text NOT NULL,
    decided_by          text,
    reason              text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (kind = 'activation' OR backfill_days IS NULL)
);
CREATE UNIQUE INDEX ctx_policy_requests_pending ON ctx_policy_requests(objective_id) WHERE state = 'pending';
CREATE INDEX ctx_policy_requests_objective ON ctx_policy_requests(objective_id, created_at DESC);

-- 每次激活与回滚（生效边界的审计链）：代次 → 生效版本
CREATE TABLE ctx_policy_activations (
    tenant_id        text NOT NULL,
    objective_id     text NOT NULL REFERENCES ctx_policies(objective_id),
    generation       bigint NOT NULL CHECK (generation >= 1),
    version          bigint NOT NULL,
    previous_version bigint NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('activation','rollback')),
    approval_id      text NOT NULL REFERENCES ctx_policy_requests(approval_id),
    effective_at     timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (objective_id, generation),
    FOREIGN KEY (objective_id, version) REFERENCES ctx_policy_versions(objective_id, version)
);

ALTER TABLE ctx_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_policy_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_policy_simulations ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_policy_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_policy_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE ctx_policy_activations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ctx_policies USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_policy_revisions USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_policy_simulations USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_policy_versions USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_policy_requests USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON ctx_policy_activations USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON ctx_policy_revisions, ctx_policy_simulations, ctx_policy_activations FROM ${MIAOQIA_DB_APP_ROLE};
REVOKE DELETE, TRUNCATE ON ctx_policies, ctx_policy_versions, ctx_policy_requests FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE ctx_policy_activations;
DROP TABLE ctx_policy_requests;
DROP TABLE ctx_policy_versions;
DROP TABLE ctx_policy_simulations;
DROP TABLE ctx_policy_revisions;
DROP TABLE ctx_policies;
