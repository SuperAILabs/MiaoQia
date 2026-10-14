-- +goose Up
-- 审计条目追加式存储：应用账号只能写入与查询，不能修改或删除。
CREATE TABLE audit_entries (
    id          text PRIMARY KEY,
    event_id    text NOT NULL UNIQUE,
    tenant_id   text,
    actor       text NOT NULL,
    actor_kind  text NOT NULL,
    subsystem   text NOT NULL,
    action      text NOT NULL,
    resource    text NOT NULL,
    result      text NOT NULL,
    basis       text,
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_entries_resource ON audit_entries (resource, occurred_at);
CREATE INDEX audit_entries_tenant ON audit_entries (tenant_id, occurred_at);

-- +goose ENVSUB ON
REVOKE UPDATE, DELETE, TRUNCATE ON audit_entries FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE audit_entries;
