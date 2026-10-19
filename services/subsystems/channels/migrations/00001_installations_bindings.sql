-- +goose Up
-- 渠道安装、绑定请求、绑定与卡片链接（PRD 第1段 F4–F7；channels.md 第5节）。租户私有数据启用行级安全，按事务会话变量
-- app.tenant_id 过滤（kernel/db.Tx 设置），应用账号 NOBYPASSRLS。渠道侧入口（回调、免登）不带租户，只经下面三张不带
-- 租户隔离的路由表派生租户：企业路由、安装授权 state 与回调去重。

-- 渠道安装：每个租户每个渠道一条。authorizing 等待官方授权回调；pending_verification 待验证；enabled 已启用；disabled 已停用。
CREATE TABLE chn_installations (
    id          text PRIMARY KEY CHECK (id ~ '^chi_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id   text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    channel     text NOT NULL CHECK (channel IN ('wecom')),
    state       text NOT NULL CHECK (state IN ('authorizing', 'pending_verification', 'enabled', 'disabled')),
    version     bigint NOT NULL CHECK (version >= 1),
    corp_id     text,
    corp_name   text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    verified_at timestamptz,
    disabled_at timestamptz,
    UNIQUE (tenant_id, channel),
    UNIQUE (tenant_id, id),
    CHECK (state IN ('authorizing', 'disabled') OR corp_id IS NOT NULL)
);

-- 企业路由（不带租户隔离）：已授权且未停用的安装所属企业 → 租户。同一企业同时只能属于一个租户；停用时删除。
-- rejected_callbacks 计入渠道健康：验签失败、重放与超出时间窗的回调。
CREATE TABLE chn_corp_routes (
    channel            text NOT NULL,
    corp_id            text NOT NULL,
    tenant_id          text NOT NULL,
    cell               text NOT NULL,
    installation_id    text NOT NULL UNIQUE,
    rejected_callbacks integer NOT NULL DEFAULT 0 CHECK (rejected_callbacks >= 0),
    last_rejected_at   timestamptz,
    PRIMARY KEY (channel, corp_id)
);

-- 安装授权 state（不带租户隔离）：只保存摘要，一次性；只有 installation_version 等于安装当前版本的 state 有效。
CREATE TABLE chn_install_states (
    digest               text PRIMARY KEY CHECK (digest ~ '^[0-9a-f]{64}$'),
    tenant_id            text NOT NULL,
    cell                 text NOT NULL,
    installation_id      text NOT NULL,
    installation_version bigint NOT NULL,
    expires_at           timestamptz NOT NULL,
    used_at              timestamptz
);

-- 回调去重（不带租户隔离）：同一渠道事件只处理一次，重放不改变状态。
CREATE TABLE chn_callback_events (
    channel     text NOT NULL,
    corp_id     text NOT NULL,
    event_id    text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel, corp_id, event_id)
);

-- 绑定：渠道账号 ↔ 成员。bound 已绑定；revoking 失效中（不再签发凭据、不再投递，等待 identity 确认撤销与最后一张凭据过期）；
-- invalidated 已失效。版本：建立为1，失效时加一；revoke_confirmed_version 是 identity 已确认撤销到的版本。
-- last_credential_expires_at 是最后一张签发凭据的到期时间（业务时间），在核对绑定的同一事务中、绑定行锁下写入。
CREATE TABLE chn_bindings (
    id                         text PRIMARY KEY CHECK (id ~ '^chb_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id                  text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    installation_id            text NOT NULL,
    channel                    text NOT NULL CHECK (channel IN ('wecom')),
    corp_id                    text NOT NULL,
    channel_user_id            text NOT NULL,
    display_name               text,
    subject                    text NOT NULL,
    -- 绑定建立（请求固定会话）时成员的成员版本：成员此后退出过即为旧绑定（identity 拒绝签发；成员事件按它判断新旧）
    member_version             bigint NOT NULL CHECK (member_version >= 1),
    state                      text NOT NULL CHECK (state IN ('bound', 'revoking', 'invalidated')),
    version                    bigint NOT NULL CHECK (version >= 1),
    reason                     text CHECK (reason IN ('member_unbind', 'admin_unbind', 'member_left', 'channel_disabled')),
    note                       text,
    last_credential_expires_at timestamptz,
    revoke_confirmed_version   bigint NOT NULL DEFAULT 0,
    revoke_attempted_at        timestamptz,
    bound_at                   timestamptz NOT NULL,
    revoking_at                timestamptz,
    invalidated_at             timestamptz,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, installation_id) REFERENCES chn_installations (tenant_id, id),
    CHECK ((state = 'bound') = (reason IS NULL))
);
-- 一个渠道账号只绑定一个成员，一个成员在同一渠道只绑定一个账号（只约束有效绑定）
CREATE UNIQUE INDEX chn_bindings_account ON chn_bindings (channel, corp_id, channel_user_id) WHERE state = 'bound';
CREATE UNIQUE INDEX chn_bindings_member ON chn_bindings (tenant_id, channel, subject) WHERE state = 'bound';
CREATE INDEX chn_bindings_list ON chn_bindings (tenant_id, created_at DESC, id DESC);
CREATE INDEX chn_bindings_revoking ON chn_bindings (tenant_id, revoke_attempted_at NULLS FIRST, id) WHERE state = 'revoking';

-- 绑定请求：一次性绑定链接（只保存令牌摘要），10分钟有效，首次登录时固定到 Web 会话与成员。
CREATE TABLE chn_binding_requests (
    id              text PRIMARY KEY CHECK (id ~ '^cbr_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id       text NOT NULL,
    installation_id text NOT NULL,
    channel         text NOT NULL CHECK (channel IN ('wecom')),
    corp_id         text NOT NULL,
    channel_user_id text NOT NULL,
    display_name    text,
    token_digest    text NOT NULL UNIQUE CHECK (token_digest ~ '^[0-9a-f]{64}$'),
    state           text NOT NULL CHECK (state IN ('initiated', 'pinned', 'completed', 'voided')),
    pinned_subject  text,
    pinned_session  text,
    pinned_member_version bigint CHECK (pinned_member_version >= 1),
    void_reason     text,
    binding_id      text,
    result          jsonb,
    expires_at      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, installation_id) REFERENCES chn_installations (tenant_id, id),
    CHECK (state IN ('initiated', 'voided') OR (pinned_subject IS NOT NULL AND pinned_session IS NOT NULL AND pinned_member_version IS NOT NULL)),
    CHECK ((state = 'completed') = (binding_id IS NOT NULL AND result IS NOT NULL))
);
CREATE INDEX chn_binding_requests_account ON chn_binding_requests (tenant_id, corp_id, channel_user_id) WHERE state = 'initiated';

-- 卡片详情链接：只指向对象，不携带登录身份；只保存令牌摘要。
CREATE TABLE chn_card_links (
    id           text PRIMARY KEY CHECK (id ~ '^lnk_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id    text NOT NULL,
    token_digest text NOT NULL UNIQUE CHECK (token_digest ~ '^[0-9a-f]{64}$'),
    recipient    text NOT NULL,
    object       text NOT NULL,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- 命令幂等：同一主体、租户与 Idempotency-Key 返回首次结果；键相同而请求不同拒绝。与业务写入、发件箱、审计同事务。
CREATE TABLE chn_commands (
    tenant_id   text NOT NULL,
    actor       text NOT NULL,
    request_key text NOT NULL CHECK (request_key ~ '^[A-Za-z0-9_-]{16,128}$'),
    operation   text NOT NULL,
    fingerprint text NOT NULL,
    response    jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, actor, request_key)
);

ALTER TABLE chn_installations ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_binding_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_card_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE chn_commands ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chn_installations USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_bindings USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_binding_requests USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_card_links USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY tenant_isolation ON chn_commands USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- +goose ENVSUB ON
REVOKE DELETE, TRUNCATE ON chn_installations, chn_bindings, chn_binding_requests, chn_card_links, chn_commands, chn_install_states, chn_callback_events FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE chn_commands;
DROP TABLE chn_card_links;
DROP TABLE chn_binding_requests;
DROP TABLE chn_bindings;
DROP TABLE chn_callback_events;
DROP TABLE chn_install_states;
DROP TABLE chn_corp_routes;
DROP TABLE chn_installations;
