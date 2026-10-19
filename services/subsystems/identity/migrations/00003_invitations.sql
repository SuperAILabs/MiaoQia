-- +goose Up
-- 邀请（PRD 第1段 F2；identity.md 第5节）。邀请不授予任何权限：接受时只能按邀请限定的租户角色与单元角色写入成员资格。
-- 状态变化（接受、撤回、重发、过期）在邀请行锁下按版本串行。链接令牌只保存 SHA-256 摘要；每次发出的链接都登记在
-- identity_invitation_links 中，重发后旧链接仍能识别为“已被替换”，但只有代次等于邀请当前代次的链接可以接受。
CREATE TABLE identity_invitations (
 id text PRIMARY KEY CHECK (id ~ '^inv_[0-9A-HJKMNP-TV-Z]{26}$'),
 tenant_id text NOT NULL REFERENCES identity_tenants(id),
 email text NOT NULL CHECK (email = lower(email) AND length(email) BETWEEN 3 AND 254),
 name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
 roles text[] NOT NULL CHECK (roles <@ ARRAY['admin','decision_maker','business_developer','pursuit_lead','research_analyst','domain_expert']::text[]),
 state text NOT NULL CHECK (state IN ('pending','accepted','revoked','expired')),
 version bigint NOT NULL CHECK (version >= 1),
 inviter text NOT NULL,
 link_generation bigint NOT NULL CHECK (link_generation >= 1),
 -- 业务时间：当前链接的过期时间（创建或最近一次重发起 7 天）
 expires_at timestamptz NOT NULL,
 -- 当前链接代次的邮件投递结果；pending 表示尚未确认（含发送结果未知）
 delivery text NOT NULL CHECK (delivery IN ('pending','sent','failed')),
 accepted_by text,
 accepted_at timestamptz,
 -- 首次接受的结果，同一账号以同一链接重复接受时原样返回
 accept_result jsonb,
 -- 记录时间
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (tenant_id, id),
 CHECK ((state = 'accepted') = (accepted_by IS NOT NULL AND accepted_at IS NOT NULL AND accept_result IS NOT NULL))
);
-- 同一邮箱在本租户只能有一个待接受的邀请
CREATE UNIQUE INDEX identity_invitations_pending_email ON identity_invitations(tenant_id, email) WHERE state = 'pending';
CREATE INDEX identity_invitations_tenant ON identity_invitations(tenant_id, created_at DESC);
CREATE INDEX identity_invitations_due ON identity_invitations(expires_at) WHERE state = 'pending';

-- 邀请限定的单元角色（租户管理员只能绑定在租户上）
CREATE TABLE identity_invitation_unit_roles (
 tenant_id text NOT NULL,
 invitation_id text NOT NULL,
 org_unit_id text NOT NULL,
 roles text[] NOT NULL CHECK (cardinality(roles) >= 1 AND roles <@ ARRAY['decision_maker','business_developer','pursuit_lead','research_analyst','domain_expert']::text[]),
 PRIMARY KEY (invitation_id, org_unit_id),
 FOREIGN KEY (tenant_id, invitation_id) REFERENCES identity_invitations(tenant_id, id),
 FOREIGN KEY (tenant_id, org_unit_id) REFERENCES identity_org_units(tenant_id, id)
);

-- 每次发出的链接（令牌摘要 → 邀请与代次）。令牌原文不落库。
CREATE TABLE identity_invitation_links (
 digest text PRIMARY KEY CHECK (digest ~ '^[0-9a-f]{64}$'),
 invitation_id text NOT NULL REFERENCES identity_invitations(id),
 generation bigint NOT NULL CHECK (generation >= 1),
 UNIQUE (invitation_id, generation)
);

-- 接受后写入 IdP 的租户与 cell 归属（每个已接受的邀请一行）。写入成功前令牌不带租户，访问失败关闭；
-- pending 的行由接受请求在提交后尝试，失败后由重复接受与 RetryTenantBindings 重试。
CREATE TABLE identity_tenant_bindings (
 invitation_id text PRIMARY KEY REFERENCES identity_invitations(id),
 tenant_id text NOT NULL REFERENCES identity_tenants(id),
 subject text NOT NULL,
 cell text NOT NULL,
 state text NOT NULL CHECK (state IN ('pending','done','cancelled')),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX identity_tenant_bindings_pending ON identity_tenant_bindings(updated_at) WHERE state = 'pending';

-- +goose Down
DROP TABLE identity_tenant_bindings;
DROP TABLE identity_invitation_links;
DROP TABLE identity_invitation_unit_roles;
DROP TABLE identity_invitations;
