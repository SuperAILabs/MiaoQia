-- +goose Up
-- 信号的“已转跟进”（PRD 第6段 F3 第1条；detection.md 第12节）：detection 按信号维护有效关联集合，由 pursuit 的
-- pursuit.pursuit.signal_linked.v1 加入、signal_unlinked.v1 移除，以关联标识幂等（已移除的关联标识保留为墓碑，晚到的同一关联的
-- 加入或移除不再生效）。集合从空变为非空时记录转换基线（转换前的处理状态、人工处置与原因）并转为已转跟进，conversion_state_version
-- 为转换写入后的状态版本 V；集合变空时只有状态版本仍为 V 才恢复基线，之后清除基线。
CREATE TABLE det_pursuit_links (
    link_id     text PRIMARY KEY CHECK (link_id ~ '^psl_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id   text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    signal_id   text NOT NULL,
    pursuit_id  text NOT NULL CHECK (pursuit_id ~ '^pur_[0-9A-HJKMNP-TV-Z]{26}$'),
    state       text NOT NULL CHECK (state IN ('active','removed')),
    recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX det_pursuit_links_signal ON det_pursuit_links(tenant_id, signal_id) WHERE state = 'active';
ALTER TABLE det_signals ADD COLUMN conversion_baseline jsonb;
ALTER TABLE det_signals ADD COLUMN conversion_state_version bigint;

ALTER TABLE det_pursuit_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON det_pursuit_links USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE DELETE, TRUNCATE ON det_pursuit_links FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
ALTER TABLE det_signals DROP COLUMN conversion_state_version;
ALTER TABLE det_signals DROP COLUMN conversion_baseline;
DROP TABLE det_pursuit_links;
