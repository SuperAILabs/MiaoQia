-- +goose Up
-- Platform directory: only identity accesses these cross-tenant tables. Every query is explicitly scoped.
CREATE TABLE identity_tenants (
 id text PRIMARY KEY CHECK (id ~ '^tnt_[0-9A-HJKMNP-TV-Z]{26}$'),
 name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 120),
 cell text NOT NULL CHECK (cell = 'cell-01'),
 state text NOT NULL CHECK (state IN ('active','suspended')),
 version bigint NOT NULL CHECK (version >= 1)
);
CREATE TABLE identity_members (
 tenant_id text NOT NULL REFERENCES identity_tenants(id),
 subject text NOT NULL CHECK (subject ~ '^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,127}$'),
 state text NOT NULL CHECK (state IN ('active','removed')),
 roles text[] NOT NULL CHECK (roles <@ ARRAY['admin','decision_maker','business_developer','pursuit_lead','research_analyst','domain_expert']::text[]),
 version bigint NOT NULL CHECK (version >= 1),
 CHECK (state <> 'removed' OR cardinality(roles) = 0),
 PRIMARY KEY (tenant_id, subject)
);
CREATE TABLE identity_commands (
 actor text NOT NULL,
 tenant_id text NOT NULL,
 request_key text NOT NULL CHECK (request_key ~ '^[A-Za-z0-9_-]{16,128}$'),
 operation text NOT NULL,
 fingerprint text NOT NULL,
 response jsonb NOT NULL,
 PRIMARY KEY (actor,tenant_id,request_key)
);
-- +goose Down
DROP TABLE identity_commands;
DROP TABLE identity_members;
DROP TABLE identity_tenants;
