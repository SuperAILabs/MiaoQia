-- +goose Up
-- 成员退出是免登的生命周期边界（PRD 第1段 F6：重新加入的成员须重新绑定）：left_version 是该成员最近一次退出时的成员版本
-- （0 表示从未退出）。绑定与免登会话记录绑定建立时的成员版本，不高于 left_version 的即为退出前的旧绑定，签发与核验都拒绝；
-- 重新加入只会使成员版本继续增加，不降低 left_version。
ALTER TABLE identity_members ADD COLUMN left_version bigint NOT NULL DEFAULT 0 CHECK (left_version >= 0);

-- 渠道免登会话与撤销水位（PRD 第1段 F6、F7；identity.md 第5节 IssueChannelSession / RevokeChannelSessions）。
-- 每个渠道绑定一行撤销水位：签发在一个事务中锁定该行，核验凭据未过期且绑定版本高于水位后写入会话；撤销锁定同一行，
-- 抬高水位并作废版本不高于水位的会话。两者在同一行上串行。水位只升不降。
CREATE TABLE identity_channel_revocations (
 binding_id text PRIMARY KEY CHECK (binding_id ~ '^chb_[0-9A-HJKMNP-TV-Z]{26}$'),
 tenant_id text NOT NULL REFERENCES identity_tenants(id),
 watermark bigint NOT NULL CHECK (watermark >= 0),
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- 免登会话：不带强认证，绝对时长12小时（业务时间 issued_at / expires_at，identity 时钟）。credential_id 是 channels 签发凭据的标识，
-- 同一凭据只产生一个会话。revoked_at 是作废的记录时间。
CREATE TABLE identity_channel_sessions (
 id text PRIMARY KEY CHECK (id ~ '^chs_[0-9A-HJKMNP-TV-Z]{26}$'),
 tenant_id text NOT NULL REFERENCES identity_tenants(id),
 binding_id text NOT NULL REFERENCES identity_channel_revocations(binding_id),
 binding_version bigint NOT NULL CHECK (binding_version >= 1),
 subject text NOT NULL,
 member_version bigint NOT NULL CHECK (member_version >= 1),
 credential_id text NOT NULL UNIQUE CHECK (credential_id ~ '^[A-Za-z0-9_-]{16,64}$'),
 issued_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK (expires_at > issued_at),
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX identity_channel_sessions_live ON identity_channel_sessions(tenant_id, binding_id) WHERE revoked_at IS NULL;

-- +goose Down
DROP TABLE identity_channel_sessions;
DROP TABLE identity_channel_revocations;
ALTER TABLE identity_members DROP COLUMN left_version;
