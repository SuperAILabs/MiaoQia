-- +goose Up
-- 持续跟踪（PRD 第6段 F10；pursuit.md 第4节 Pursuit 的 continuous 形态）。持续型跟进与交易型共用 pur_pursuits 的一行（标识、目标、
-- 所在单元、负责人、参与版本与授权同步、成员变化的重新判断），kind 为 continuous；交易型专有的列取固定值：fact_type 为
-- pursuit.tracking、subject_id 为跟进自身的标识、target_key 为 continuous|<标识>（不参与唯一性）、来源为 manual、渠道为 web。
-- 持续跟踪的状态（跟踪中 / 已暂停 / 已停止）、对象集合与周期在 pur_tracking；pur_pursuits.state 保持 open（参与人仍按成员变化重新判断，
-- 停止后仍可查看已有各期）。
ALTER TABLE pur_pursuits DROP CONSTRAINT pur_pursuits_kind_check;
ALTER TABLE pur_pursuits ADD CONSTRAINT pur_pursuits_kind_check CHECK (kind IN ('deal','continuous'));
CREATE INDEX pur_pursuits_kind ON pur_pursuits(tenant_id, kind, created_at DESC, id DESC);

-- 持续跟踪的计划：对象集合（组织标识，至多 200 个）与成员版本（每次修改对象集合加一）；周期为每周 weekday（1 周一 … 7 周日）的
-- run_time，按 timezone 计算。跟踪中时 period_start 为进行中的一期的开始（创建或恢复的时刻，或上一期的结束），next_run_at 为它的结束
-- （下一个周期时点）；暂停与停止时两者为空（暂停期间不生成、恢复后不补）。last_seq 为最近结束的一期的序号。
CREATE TABLE pur_tracking (
    pursuit_id      text PRIMARY KEY REFERENCES pur_pursuits(id),
    tenant_id       text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    name            text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    state           text NOT NULL CHECK (state IN ('tracking','paused','stopped')),
    members         text[] NOT NULL CHECK (cardinality(members) BETWEEN 1 AND 200),
    members_version bigint NOT NULL CHECK (members_version >= 1),
    weekday         integer NOT NULL CHECK (weekday BETWEEN 1 AND 7),
    run_time        text NOT NULL CHECK (run_time ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    timezone        text NOT NULL,
    period_start    timestamptz,
    next_run_at     timestamptz,
    last_seq        bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    stopped_at      timestamptz,
    CHECK ((state = 'tracking') = (next_run_at IS NOT NULL) AND (next_run_at IS NULL) = (period_start IS NULL)),
    CHECK ((state = 'stopped') = (stopped_at IS NOT NULL))
);
CREATE INDEX pur_tracking_due ON pur_tracking(tenant_id, next_run_at) WHERE state = 'tracking';

-- 已结束的各期（追加式）：实际期间 [period_start, period_end)、期末的成员快照与成员版本、对比基准（上一期）与期末的负责人。
-- 快照在该期结束时固定，之后修改对象集合不改变它；读取时按查看者当前的权限判断，不以快照中的负责人为准。
CREATE TABLE pur_tracking_periods (
    tenant_id       text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    pursuit_id      text NOT NULL REFERENCES pur_pursuits(id),
    seq             bigint NOT NULL CHECK (seq >= 1),
    period_start    timestamptz NOT NULL,
    period_end      timestamptz NOT NULL,
    timezone        text NOT NULL,
    members         text[] NOT NULL,
    members_version bigint NOT NULL CHECK (members_version >= 1),
    baseline_seq    bigint CHECK (baseline_seq >= 1 AND baseline_seq < seq),
    owner           text NOT NULL,
    owner_valid     boolean NOT NULL,
    briefing_state  text NOT NULL DEFAULT 'requested' CHECK (briefing_state IN ('requested')),
    recorded_at     timestamptz NOT NULL,
    PRIMARY KEY (pursuit_id, seq),
    CHECK (period_start < period_end),
    CHECK ((seq = 1) = (baseline_seq IS NULL))
);

ALTER TABLE pur_tracking ENABLE ROW LEVEL SECURITY;
ALTER TABLE pur_tracking_periods ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pur_tracking USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON pur_tracking_periods USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON pur_tracking_periods FROM ${MIAOQIA_DB_APP_ROLE};
REVOKE DELETE, TRUNCATE ON pur_tracking FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE pur_tracking_periods;
DROP TABLE pur_tracking;
DROP INDEX pur_pursuits_kind;
ALTER TABLE pur_pursuits DROP CONSTRAINT pur_pursuits_kind_check;
ALTER TABLE pur_pursuits ADD CONSTRAINT pur_pursuits_kind_check CHECK (kind IN ('deal'));
