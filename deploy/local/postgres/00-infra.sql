-- 可选 Docker 引导环境使用的基础组件数据库；共享开发环境不依赖本文件。
CREATE ROLE keycloak LOGIN PASSWORD 'keycloak';
CREATE DATABASE keycloak OWNER keycloak;
CREATE ROLE openfga LOGIN PASSWORD 'openfga';
CREATE DATABASE openfga OWNER openfga;
