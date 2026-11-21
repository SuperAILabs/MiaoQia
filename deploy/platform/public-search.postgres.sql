-- Run once as the PostgreSQL cluster administrator with psql -v ON_ERROR_STOP=1.
-- Provision LOGIN credentials for these roles separately through the secret owner.
CREATE ROLE sea_public_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE ROLE sea_public_app NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
CREATE DATABASE sea_public OWNER sea_public_owner;
REVOKE ALL ON DATABASE sea_public FROM PUBLIC;
GRANT CONNECT ON DATABASE sea_public TO sea_public_app;
\connect sea_public
ALTER SCHEMA public OWNER TO sea_public_owner;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO sea_public_app;
ALTER DEFAULT PRIVILEGES FOR ROLE sea_public_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO sea_public_app;
ALTER DEFAULT PRIVILEGES FOR ROLE sea_public_owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO sea_public_app;
