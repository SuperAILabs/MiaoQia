-- +goose Up
-- 租户反馈集（PRD 第3段 F7；learning.md 第4节 Feedback）：由 detection.signal.detected.v1 与 detection.signal.triaged.v1 写入，
-- 以事件与操作标识幂等，正确性不依赖 detection 同步调用 learning。租户私有数据：行级安全按 app.tenant_id 过滤。

-- 信号的汇总口径字段：系统处置（排除信号单独统计）、发现时间（窗口）、当前内容版本（取已收到事件中的最大值）。
CREATE TABLE lrn_signals (
    tenant_id       text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    signal_id       text PRIMARY KEY CHECK (signal_id ~ '^sig_[0-9A-HJKMNP-TV-Z]{26}$'),
    objective_id    text NOT NULL,
    objective_code  text,
    disposition     text NOT NULL CHECK (disposition IN ('pursue_candidate','review_candidate','watch_signal','excluded')),
    discovered_at   timestamptz NOT NULL,
    content_version bigint NOT NULL DEFAULT 0 CHECK (content_version >= 0)
);
CREATE INDEX lrn_signals_window ON lrn_signals(tenant_id, objective_id, discovered_at);

-- 成员的当前反馈：只接受版本更高的变化（事件乱序不回退）；撤回为 result 为空。
CREATE TABLE lrn_feedback (
    tenant_id   text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    signal_id   text NOT NULL,
    member      text NOT NULL,
    result      text CHECK (result IN ('relevant','irrelevant','exclusion_correct','exclusion_wrong')),
    reasons     text[] NOT NULL DEFAULT '{}',
    based_on_content_version bigint NOT NULL DEFAULT 0,
    version     bigint NOT NULL CHECK (version >= 1),
    recorded_at timestamptz NOT NULL,
    PRIMARY KEY (signal_id, member)
);

-- 已计入的处理操作（以操作标识幂等，追加式）。
CREATE TABLE lrn_feedback_ops (
    tenant_id    text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    operation_id text NOT NULL,
    signal_id    text NOT NULL,
    occurred_at  timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, operation_id)
);

ALTER TABLE lrn_signals ENABLE ROW LEVEL SECURITY;
ALTER TABLE lrn_feedback ENABLE ROW LEVEL SECURITY;
ALTER TABLE lrn_feedback_ops ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON lrn_signals USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON lrn_feedback USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON lrn_feedback_ops USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON lrn_feedback_ops FROM ${MIAOQIA_DB_APP_ROLE};
REVOKE DELETE, TRUNCATE ON lrn_signals, lrn_feedback FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE lrn_feedback_ops;
DROP TABLE lrn_feedback;
DROP TABLE lrn_signals;
