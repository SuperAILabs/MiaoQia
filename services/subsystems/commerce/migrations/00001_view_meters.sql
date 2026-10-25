-- +goose Up
-- 对象查看与原文查看的计量（PRD 第4段 F9、F10；commerce.md 第6节）。按租户隔离：行级安全按事务会话变量 app.tenant_id 过滤。
-- 计量日按租户时区的日期（缺省 Asia/Shanghai）。

-- 每租户、种类与计量日的已用量。计入与额度检查在同一事务中按行锁串行化：额度用完时不写入计量操作。
CREATE TABLE com_view_usage (
    tenant_id    text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    kind         text NOT NULL CHECK (kind IN ('object_view','evidence_view')),
    metering_day date NOT NULL,
    used         bigint NOT NULL DEFAULT 0 CHECK (used >= 0),
    PRIMARY KEY (tenant_id, kind, metering_day)
);

-- 计量操作（追加式）：（租户、查看者、种类、对象或证据、计量日）的摘要为键，同一计量操作只计一次；source 区分在线计入与补记。
CREATE TABLE com_view_meters (
    tenant_id    text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    meter_key    text NOT NULL CHECK (meter_key ~ '^mtr:[0-9a-f]{64}$'),
    kind         text NOT NULL CHECK (kind IN ('object_view','evidence_view')),
    target       text NOT NULL,
    member       text NOT NULL,
    metering_day date NOT NULL,
    source       text NOT NULL CHECK (source IN ('online','backfill')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, meter_key)
);

-- 尝试（追加式）：一次页面请求的键（按查看者区分）与该尝试的固定结果（计入、无变化或 quota.exhausted）；暂时失败不登记。
CREATE TABLE com_view_attempts (
    tenant_id   text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    actor       text NOT NULL,
    request_key text NOT NULL,
    fingerprint text NOT NULL,
    status      integer NOT NULL CHECK (status IN (200, 429)),
    response    jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, actor, request_key)
);

ALTER TABLE com_view_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE com_view_meters ENABLE ROW LEVEL SECURITY;
ALTER TABLE com_view_attempts ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON com_view_usage USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON com_view_meters USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON com_view_attempts USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON com_view_meters, com_view_attempts FROM ${MIAOQIA_DB_APP_ROLE};
REVOKE DELETE, TRUNCATE ON com_view_usage FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE com_view_attempts;
DROP TABLE com_view_meters;
DROP TABLE com_view_usage;
