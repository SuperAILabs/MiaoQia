-- +goose Up
-- 跟进结果（PRD 第6段 F7 第5条；learning.md 第8.1节）：由 pursuit.outcome.recorded.v1 写入，以结果记录标识幂等，只用于本租户的评估。
-- 有获知时间的按 known_at_utc 计入时点评估（basis = point_in_time），获知时间缺失的只进入事后统计（post_hoc）。修订产生新记录并引用被取代
-- 的记录，不改写已有记录与已发布的报告。结果不改变策略、信号或公共事实。
CREATE TABLE lrn_outcomes (
    outcome_id       text PRIMARY KEY CHECK (outcome_id ~ '^otc_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id        text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    pursuit_id       text NOT NULL,
    objective_id     text NOT NULL,
    template_id      text NOT NULL,
    result           text NOT NULL CHECK (result IN ('success','failure','abandoned')),
    reason_code      text NOT NULL,
    known_at_utc     timestamptz,
    basis            text NOT NULL CHECK (basis IN ('point_in_time','post_hoc')),
    contribution     text NOT NULL,
    source_signal_id text,
    supersedes       text,
    amounts          jsonb NOT NULL,
    recorded_at      timestamptz NOT NULL,
    received_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((basis = 'point_in_time') = (known_at_utc IS NOT NULL))
);
CREATE INDEX lrn_outcomes_objective ON lrn_outcomes(tenant_id, objective_id, known_at_utc);

ALTER TABLE lrn_outcomes ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON lrn_outcomes USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON lrn_outcomes FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE lrn_outcomes;
