-- Reverse initial schema migration (drop tables in reverse dependency order)

DROP TABLE IF EXISTS system_settings;
DROP TABLE IF EXISTS oidc_config;
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS organization_members;
DROP TABLE IF EXISTS role_templates;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;
