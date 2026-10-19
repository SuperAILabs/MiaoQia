-- +goose Up
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
