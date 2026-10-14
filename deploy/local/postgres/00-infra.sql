-- 基础组件使用的数据库（仅本地开发环境）。
CREATE ROLE keycloak LOGIN PASSWORD 'keycloak';
CREATE DATABASE keycloak OWNER keycloak;
CREATE ROLE openfga LOGIN PASSWORD 'openfga';
CREATE DATABASE openfga OWNER openfga;
