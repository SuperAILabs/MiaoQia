-- +goose Up
-- 策略仿真运行（PRD 第7段 F3；learning.md 第4、5节 SimulationRun）。租户私有数据：行级安全按事务会话变量 app.tenant_id 过滤。
-- 运行结束即写入（同步运行，不保存进行中状态）：完成的运行带固定输入与报告，失败的运行带失败原因；内容不可变。
-- input_key 是固定输入（目标、草案修订、对比版本与策略代次、上下文版本、数据集版本、两侧启用领域集合、窗口）的摘要：
-- 同一租户同一输入只有一个完成的运行，相同输入的重试直接复用。失败的运行没有输入键（可能在输入确定之前失败），不复用。
CREATE TABLE lrn_simulation_runs (
    id             text PRIMARY KEY CHECK (id ~ '^sim_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id      text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    objective_id   text NOT NULL,
    revision       bigint NOT NULL CHECK (revision >= 1),
    status         text NOT NULL CHECK (status IN ('completed','failed')),
    failure_reason text CHECK (failure_reason IN ('dataset_unavailable','domain_status_unavailable','context_unavailable','policy_unavailable','timeout')),
    input_key      text,
    inputs         jsonb,
    report         jsonb,
    -- 发起方：用户主体或 svc:learning（草案保存后的自动仿真）
    requested_by   text NOT NULL,
    -- 业务时间：运行结束
    completed_at   timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'failed') = (failure_reason IS NOT NULL)),
    CHECK (status = 'failed' OR (input_key IS NOT NULL AND inputs IS NOT NULL AND report IS NOT NULL))
);
CREATE UNIQUE INDEX lrn_simulation_runs_input ON lrn_simulation_runs(tenant_id, input_key) WHERE status = 'completed';
CREATE INDEX lrn_simulation_runs_revision ON lrn_simulation_runs(objective_id, revision, completed_at DESC);

ALTER TABLE lrn_simulation_runs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON lrn_simulation_runs USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON lrn_simulation_runs FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE lrn_simulation_runs;
