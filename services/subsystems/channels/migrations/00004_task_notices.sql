-- +goose Up
-- 渠道通知（PRD 第6段 F9 第2–3条、F12 第6条；C-09）：任务提醒卡片（消费 pursuit.task.due.v1）与授权被拒时给操作者的通知
-- （消费 pursuit.pursuit.authorization_rejected.v1）。每个（来源、收件人）一行：source 为提醒标识（rmd_）或“跟进/参与版本”；
-- recipient 为 '*' 的行是转给决策者的提醒（escalated）尚未展开：投递时以每个已绑定成员本人经 pursuit getTask 判断（viewer_escalated），
-- 展开为各自的一行。消息在第一次发送前渲染并固定（重试发送同一张卡片，令牌不变）；每次发送前按收件人当前状态再检查。
-- state：pending 待发送（含安静时段延后）/ retrying 退避重试 / delivered 渠道已接受 / in_app 未绑定或截止后仍失败 /
-- cancelled 发送前再检查时取消（reason）/ expanded 已展开（只用于 recipient '*'）。
CREATE TABLE chn_notices (
    id               text PRIMARY KEY CHECK (id ~ '^dlv_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id        text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    kind             text NOT NULL CHECK (kind IN ('task_due', 'authorization_rejected')),
    source           text NOT NULL,
    recipient        text NOT NULL,
    pursuit_id       text NOT NULL CHECK (pursuit_id ~ '^pur_[0-9A-HJKMNP-TV-Z]{26}$'),
    task_id          text CHECK (task_id ~ '^tsk_[0-9A-HJKMNP-TV-Z]{26}$'),
    deadline_version bigint,
    escalated        boolean NOT NULL DEFAULT false,
    fire_at          timestamptz,
    payload          jsonb NOT NULL,
    state            text NOT NULL CHECK (state IN ('pending', 'retrying', 'delivered', 'in_app', 'cancelled', 'expanded')),
    reason           text,
    binding_id       text,
    corp_id          text,
    channel_user_id  text,
    message          jsonb,
    attempts         integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  timestamptz NOT NULL,
    cutoff_at        timestamptz NOT NULL,
    created_at       timestamptz NOT NULL,
    submitted_at     timestamptz,
    accepted_at      timestamptz,
    UNIQUE (tenant_id, kind, source, recipient),
    CHECK ((kind = 'task_due') = (task_id IS NOT NULL AND deadline_version IS NOT NULL)),
    CHECK ((message IS NULL) = (binding_id IS NULL))
);
CREATE INDEX chn_notices_due ON chn_notices (tenant_id, next_attempt_at) WHERE state IN ('pending', 'retrying');
CREATE INDEX chn_notices_task ON chn_notices (tenant_id, task_id, recipient);

-- 动作令牌追加任务提醒卡片上的“完成”（complete_task）与“撤销”（reopen_task）：令牌带任务与提醒所在的截止版本，不带信号；
-- 撤销令牌的 target_operation 为被撤销的“完成”的令牌（= 操作标识）。
ALTER TABLE chn_action_tokens ALTER COLUMN signal_id DROP NOT NULL;
ALTER TABLE chn_action_tokens ADD COLUMN task_id text CHECK (task_id ~ '^tsk_[0-9A-HJKMNP-TV-Z]{26}$');
ALTER TABLE chn_action_tokens ADD COLUMN deadline_version bigint;
ALTER TABLE chn_action_tokens ADD COLUMN target_operation text;
ALTER TABLE chn_action_tokens DROP CONSTRAINT chn_action_tokens_action_check;
ALTER TABLE chn_action_tokens ADD CONSTRAINT chn_action_tokens_action_check
    CHECK (action IN ('relevant', 'irrelevant', 'undo', 'pursue', 'complete_task', 'reopen_task'));
ALTER TABLE chn_action_tokens ADD CONSTRAINT chn_action_tokens_subject_check
    CHECK ((action IN ('complete_task', 'reopen_task')) = (task_id IS NOT NULL) AND (task_id IS NULL) = (signal_id IS NOT NULL));
ALTER TABLE chn_action_tokens ADD CONSTRAINT chn_action_tokens_task_target_check
    CHECK ((action = 'reopen_task') = (target_operation IS NOT NULL));

-- 调度器的到期组装（Briefings.ComposeDue）：组装后没有生成简报的成员（不是有效成员、不是本期受众）记一次，同一期不再组装；
-- 已生成简报的以 chn_briefings 为准。组装失败的不记（下一次调用重试）。
CREATE TABLE chn_compose_marks (
    tenant_id   text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    subject     text NOT NULL,
    source      text NOT NULL,
    period_end  timestamptz NOT NULL,
    outcome     text NOT NULL CHECK (outcome IN ('skipped')),
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, subject, source, period_end)
);
ALTER TABLE chn_compose_marks ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chn_compose_marks USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE chn_notices ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chn_notices USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE DELETE, TRUNCATE ON chn_notices FROM ${MIAOQIA_DB_APP_ROLE};
REVOKE DELETE, TRUNCATE ON chn_compose_marks FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE chn_compose_marks;
ALTER TABLE chn_action_tokens DROP CONSTRAINT chn_action_tokens_task_target_check;
ALTER TABLE chn_action_tokens DROP CONSTRAINT chn_action_tokens_subject_check;
ALTER TABLE chn_action_tokens DROP CONSTRAINT chn_action_tokens_action_check;
ALTER TABLE chn_action_tokens ADD CONSTRAINT chn_action_tokens_action_check CHECK (action IN ('relevant', 'irrelevant', 'undo', 'pursue'));
ALTER TABLE chn_action_tokens DROP COLUMN target_operation;
ALTER TABLE chn_action_tokens DROP COLUMN deadline_version;
ALTER TABLE chn_action_tokens DROP COLUMN task_id;
ALTER TABLE chn_action_tokens ALTER COLUMN signal_id SET NOT NULL;
DROP TABLE chn_notices;
