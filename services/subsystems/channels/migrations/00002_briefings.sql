-- +goose Up
-- 简报、投递、动作令牌、简报链接与偏好（PRD 第5段；channels.md 第4.4、5节）。全部为租户私有数据，启用行级安全；简报、投递、
-- 令牌与链接只属于一名成员（recipient / subject），按成员过滤在用例层，按租户过滤在行级安全。

-- 偏好版本：每次修改追加一版；effective_from 为新版本生效的周期起点（修改从下一期生效，F9）。从未保存时使用缺省值（版本 0）。
CREATE TABLE chn_briefing_preferences (
    tenant_id      text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    subject        text NOT NULL,
    version        bigint NOT NULL CHECK (version >= 1),
    send_time      text NOT NULL CHECK (send_time ~ '^([01][0-9]|2[0-3]):(00|30)$'),
    timezone       text NOT NULL,
    send_days      integer[] NOT NULL,
    quiet_start    text NOT NULL CHECK (quiet_start ~ '^([01][0-9]|2[0-3]):(00|30)$'),
    quiet_end      text NOT NULL CHECK (quiet_end ~ '^([01][0-9]|2[0-3]):(00|30)$'),
    push_empty     boolean NOT NULL,
    section_limits jsonb NOT NULL DEFAULT '{}',
    effective_from timestamptz NOT NULL,
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, subject, version)
);

-- 一期简报。幂等键：租户 + 成员 + 简报来源 + 周期起止（F1 第3条）。sections 为组装时的快照（契约 BriefingSection 的数组）。
-- push_state：pushing 投递中 / delivered 已送达 / in_app 推送失败已在站内 / not_pushed 未推送（not_pushed_reason）。
CREATE TABLE chn_briefings (
    id                 text PRIMARY KEY CHECK (id ~ '^brf_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id          text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    subject            text NOT NULL,
    source             text NOT NULL CHECK (source ~ '^BRF-[A-Z0-9-]{1,64}$'),
    period_start       timestamptz NOT NULL,
    period_end         timestamptz NOT NULL,
    timezone           text NOT NULL,
    assembled_at       timestamptz NOT NULL,
    delayed            boolean NOT NULL,
    empty              boolean NOT NULL,
    partial            boolean NOT NULL,
    rule_version       text NOT NULL,
    preference_version bigint NOT NULL CHECK (preference_version >= 0),
    sections           jsonb NOT NULL,
    item_count         integer NOT NULL CHECK (item_count >= 0),
    push_state         text NOT NULL CHECK (push_state IN ('pushing', 'delivered', 'in_app', 'not_pushed')),
    not_pushed_reason  text CHECK (not_pushed_reason IN ('empty', 'not_bound', 'late')),
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, subject, source, period_start, period_end),
    UNIQUE (tenant_id, id),
    CHECK (period_start < period_end),
    CHECK ((push_state = 'not_pushed') = (not_pushed_reason IS NOT NULL))
);
CREATE INDEX chn_briefings_history ON chn_briefings (tenant_id, subject, period_end DESC, id DESC);

-- 每个（成员, 目标）区上一次成功取数并随简报保存的终点（F1 第5条）：只有取数成功后才前移。
CREATE TABLE chn_briefing_windows (
    tenant_id    text NOT NULL,
    subject      text NOT NULL,
    objective_id text NOT NULL CHECK (objective_id ~ '^obj_[0-9A-HJKMNP-TV-Z]{26}$'),
    fetched_to   timestamptz NOT NULL,
    briefing_id  text NOT NULL,
    PRIMARY KEY (tenant_id, subject, objective_id)
);

-- 一次投递（简报推送或本人重新推送）：messages 为已渲染的消息（重试发送同一批消息，卡片与令牌不变）；sent 为已被渠道接受的条数。
-- state：pending 待发送（含安静时段延后）/ retrying 退避重试 / delivered 渠道已接受 / in_app 截止后仍失败 / cancelled 发送前再检查时取消。
CREATE TABLE chn_deliveries (
    id              text PRIMARY KEY CHECK (id ~ '^dlv_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id       text NOT NULL,
    briefing_id     text NOT NULL,
    recipient       text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('briefing', 'resend')),
    binding_id      text NOT NULL,
    corp_id         text NOT NULL,
    channel_user_id text NOT NULL,
    state           text NOT NULL CHECK (state IN ('pending', 'retrying', 'delivered', 'in_app', 'cancelled')),
    reason          text CHECK (reason IN ('channel_error', 'cutoff', 'member_inactive', 'binding_inactive', 'channel_not_enabled')),
    messages        jsonb NOT NULL,
    sections        jsonb NOT NULL,
    sent            integer NOT NULL DEFAULT 0 CHECK (sent >= 0),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    dropped_sections integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    cutoff_at       timestamptz NOT NULL,
    created_at      timestamptz NOT NULL,
    submitted_at    timestamptz,
    accepted_at     timestamptz,
    FOREIGN KEY (tenant_id, briefing_id) REFERENCES chn_briefings (tenant_id, id)
);
CREATE INDEX chn_deliveries_due ON chn_deliveries (tenant_id, next_attempt_at) WHERE state IN ('pending', 'retrying');
CREATE INDEX chn_deliveries_briefing ON chn_deliveries (tenant_id, briefing_id, created_at);

-- 动作令牌（= 操作标识，F4 第5条、F5）。卡片上只带签名的令牌值（channels 签发，整个载荷连同 HMAC 签名），本表保存其余状态：
-- 所见内容版本、到期时间、所属卡片与投递、首次执行时固定的请求（同一操作的每次尝试发送同一请求）、尝试次数与确定结果。
-- state：open 有效 / settled 已得到确定结果（作废，再点返回已固定的结果）。暂时失败不作废。
CREATE TABLE chn_action_tokens (
    id              text PRIMARY KEY CHECK (id ~ '^atk_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id       text NOT NULL,
    recipient       text NOT NULL,
    signal_id       text NOT NULL CHECK (signal_id ~ '^sig_[0-9A-HJKMNP-TV-Z]{26}$'),
    objective_id    text NOT NULL,
    action          text NOT NULL CHECK (action IN ('relevant', 'irrelevant', 'undo')),
    reason          text,
    content_version bigint NOT NULL CHECK (content_version >= 0),
    target_token    text,
    card_id         text NOT NULL,
    delivery_id     text,
    expires_at      timestamptz NOT NULL,
    state           text NOT NULL CHECK (state IN ('open', 'settled')),
    request         jsonb,
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    result          jsonb,
    executed_at     timestamptz,
    created_at      timestamptz NOT NULL,
    CHECK ((action = 'undo') = (target_token IS NOT NULL)),
    CHECK ((state = 'settled') = (result IS NOT NULL AND executed_at IS NOT NULL))
);

-- 待执行的卡片点击（回调在 1 秒内确认、异步执行，F5 第2条）。一次点击一行；操作开始前的拒绝只结束本行，不触及令牌。
CREATE TABLE chn_action_jobs (
    id              text PRIMARY KEY,
    tenant_id       text NOT NULL,
    token_id        text NOT NULL,
    corp_id         text NOT NULL,
    channel_user_id text NOT NULL,
    card_id         text NOT NULL,
    event_id        text NOT NULL,
    state           text NOT NULL CHECK (state IN ('queued', 'done')),
    outcome         text,
    received_at     timestamptz NOT NULL,
    done_at         timestamptz
);
CREATE INDEX chn_action_jobs_queued ON chn_action_jobs (tenant_id, received_at) WHERE state = 'queued';

-- 简报与卡片上的链接（F6）：只指向对象（信号、简报、审批），不携带登录身份；只保存令牌摘要；7 天有效。
CREATE TABLE chn_briefing_links (
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL,
    token_digest text NOT NULL UNIQUE CHECK (token_digest ~ '^[0-9a-f]{64}$'),
    recipient    text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('signal', 'briefing', 'approval')),
    target       text NOT NULL CHECK (target ~ '^(sig|brf|apr)_[0-9A-HJKMNP-TV-Z]{26}$'),
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL
);

ALTER TABLE chn_briefing_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_briefings ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_briefing_windows ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_action_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_action_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_briefing_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chn_briefing_preferences USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_briefings USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_briefing_windows USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_deliveries USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_action_tokens USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_action_jobs USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_briefing_links USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE DELETE, TRUNCATE ON chn_briefing_preferences, chn_briefings, chn_briefing_windows, chn_deliveries, chn_action_tokens, chn_action_jobs, chn_briefing_links FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE chn_briefing_links;
DROP TABLE chn_action_jobs;
DROP TABLE chn_action_tokens;
DROP TABLE chn_deliveries;
DROP TABLE chn_briefing_windows;
DROP TABLE chn_briefings;
DROP TABLE chn_briefing_preferences;
