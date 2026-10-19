-- +goose Up
-- 组织单元、单元角色与业务资源的参与关系登记（identity.md 7.1、SyncResourceParticipants）。
-- 每个租户有一个根单元，标识为保留值 root；其他单元为 unt_<ULID>。单元层级、成员资格与角色绑定以本库为准，
-- 授权决策时作为上下文关系交给 OpenFGA 求值；参与关系本身持久化在 OpenFGA，本库只记录资源所在单元与已应用的参与版本。
CREATE TABLE identity_org_units (
 tenant_id text NOT NULL REFERENCES identity_tenants(id),
 id text NOT NULL CHECK (id = 'root' OR id ~ '^unt_[0-9A-HJKMNP-TV-Z]{26}$'),
 parent_id text,
 name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
 version bigint NOT NULL CHECK (version >= 1),
 PRIMARY KEY (tenant_id, id),
 FOREIGN KEY (tenant_id, parent_id) REFERENCES identity_org_units(tenant_id, id),
 CHECK ((id = 'root') = (parent_id IS NULL)),
 CHECK (parent_id IS DISTINCT FROM id)
);
-- 根单元随租户创建（含直接写入目录的初始化与已有租户）。
-- +goose StatementBegin
CREATE FUNCTION identity_create_root_unit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO identity_org_units(tenant_id, id, parent_id, name, version) VALUES (NEW.id, 'root', NULL, NEW.name, 1) ON CONFLICT DO NOTHING;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER identity_tenant_root_unit AFTER INSERT ON identity_tenants FOR EACH ROW EXECUTE FUNCTION identity_create_root_unit();
INSERT INTO identity_org_units(tenant_id, id, parent_id, name, version) SELECT id, 'root', NULL, name, 1 FROM identity_tenants ON CONFLICT DO NOTHING;

-- 绑定在组织单元上的角色。租户管理员只绑定在租户上（identity_members.roles）。
CREATE TABLE identity_member_unit_roles (
 tenant_id text NOT NULL,
 subject text NOT NULL,
 org_unit_id text NOT NULL,
 roles text[] NOT NULL CHECK (cardinality(roles) >= 1 AND roles <@ ARRAY['decision_maker','business_developer','pursuit_lead','research_analyst','domain_expert']::text[]),
 PRIMARY KEY (tenant_id, subject, org_unit_id),
 FOREIGN KEY (tenant_id, subject) REFERENCES identity_members(tenant_id, subject),
 FOREIGN KEY (tenant_id, org_unit_id) REFERENCES identity_org_units(tenant_id, id)
);

-- 业务资源登记：资源所在租户与单元、已应用的参与版本、该版本的请求指纹与结果（同一版本重放返回同一结果）。
-- 资源标识全局唯一（<类型>:<前缀>_<ULID>），一经登记不能转到其他租户。
CREATE TABLE identity_resources (
 object text PRIMARY KEY CHECK (object ~ '^(objective:obj|pursuit:pur|research_case:rsc)_[0-9A-HJKMNP-TV-Z]{26}$'),
 tenant_id text NOT NULL REFERENCES identity_tenants(id),
 org_unit_id text NOT NULL,
 applied_version bigint NOT NULL CHECK (applied_version >= 0),
 fingerprint text NOT NULL,
 result jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY (tenant_id, org_unit_id) REFERENCES identity_org_units(tenant_id, id)
);
CREATE INDEX identity_resources_tenant ON identity_resources(tenant_id);

-- +goose Down
DROP TABLE identity_resources;
DROP TABLE identity_member_unit_roles;
DROP TRIGGER identity_tenant_root_unit ON identity_tenants;
DROP FUNCTION identity_create_root_unit();
DROP TABLE identity_org_units;
