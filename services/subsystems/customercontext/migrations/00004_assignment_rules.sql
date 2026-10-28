-- +goose Up
-- 目标的分派规则（customer-context.md 第11节，PRD 第6段 F1 第2条，C-14）：每个目标一份按顺序的规则，整份替换，分派配置版本独立于目标版本
-- （从未配置为 0，第一次写入为 1）。写入与调整负责人、参与人在同一目标行锁下串行（规则的被分派人须是该目标当前的负责人或参与人）。
CREATE TABLE ctx_assignment_rules (
    objective_id text PRIMARY KEY REFERENCES ctx_objectives(id),
    tenant_id    text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    version      bigint NOT NULL CHECK (version >= 1),
    rules        jsonb NOT NULL CHECK (jsonb_typeof(rules) = 'array'),
    updated_by   text NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE ctx_assignment_rules ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ctx_assignment_rules USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE DELETE, TRUNCATE ON ctx_assignment_rules FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE ctx_assignment_rules;
