-- +goose Up
CREATE TABLE sea_watch_queries (
    id text PRIMARY KEY CHECK (id ~ '^obj_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id text NOT NULL CHECK (tenant_id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
    cell text NOT NULL CHECK (cell ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    fact_types jsonb NOT NULL CHECK (jsonb_typeof(fact_types) = 'array'),
    regions jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(regions) = 'array'),
    categories jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(categories) = 'array'),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX sea_watch_queries_cell_idx ON sea_watch_queries (cell, tenant_id);

ALTER TABLE sea_watch_queries ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sea_watch_queries
    USING (tenant_id = current_setting('app.tenant_id', true) OR current_setting('app.principal', true) = 'svc:detection')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) OR current_setting('app.principal', true) = 'svc:detection');

-- 订阅随目标停用或删除而移除，应用账号需要删除自身生命周期内的记录；
-- TRUNCATE 仍不授予应用账号，避免绕过 RLS 清空整表。
-- +goose ENVSUB ON
REVOKE TRUNCATE ON sea_watch_queries FROM ${MIAOQIA_DB_APP_ROLE};
-- +goose ENVSUB OFF

-- +goose Down
DROP TABLE sea_watch_queries;
