-- Canonicalize TSM's public identity tables to the shared identity model
-- (terraform-suite-identity v0.11.0). This mirrors the module's identity-schema
-- migration 000003 but targets TSM's own public schema, so the v0.11.0 identity
-- store (which expects JSONB scopes, per-org IdP binding, API-key expiry tracking
-- and the registry OIDC shape) works against the public schema in the default,
-- non-cutover configuration.
--
-- Changes are additive where possible; the scopes type changes convert the
-- existing seeded values losslessly via the USING clauses. Pre-existing columns
-- the canonical model no longer maps (organizations.is_active/description,
-- users.is_active, organization_members.id/updated_at, api_keys.is_active/
-- updated_at) are intentionally left in place — the store simply ignores them.

-- organizations: per-organization IdP binding.
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS idp_type VARCHAR(50);
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS idp_name VARCHAR(255);

-- role_templates.scopes: TEXT[] -> JSONB (scopes stored as a JSON array).
ALTER TABLE role_templates ALTER COLUMN scopes DROP DEFAULT;
ALTER TABLE role_templates ALTER COLUMN scopes TYPE JSONB USING to_jsonb(scopes);
ALTER TABLE role_templates ALTER COLUMN scopes SET DEFAULT '[]'::jsonb;

-- api_keys.scopes: TEXT[] -> JSONB; add expiry-notification tracking.
ALTER TABLE api_keys ALTER COLUMN scopes DROP DEFAULT;
ALTER TABLE api_keys ALTER COLUMN scopes TYPE JSONB USING to_jsonb(scopes);
ALTER TABLE api_keys ALTER COLUMN scopes SET DEFAULT '[]'::jsonb;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS expiry_notification_sent_at TIMESTAMP;

-- oidc_config: registry shape (named, multi-provider, group-mapping extra_config,
-- audit columns) and scopes as a JSON array.
ALTER TABLE oidc_config ADD COLUMN IF NOT EXISTS name VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE oidc_config ADD COLUMN IF NOT EXISTS provider_type VARCHAR(50) NOT NULL DEFAULT 'generic_oidc';
ALTER TABLE oidc_config ADD COLUMN IF NOT EXISTS extra_config JSONB;
ALTER TABLE oidc_config ADD COLUMN IF NOT EXISTS created_by UUID;
ALTER TABLE oidc_config ADD COLUMN IF NOT EXISTS updated_by UUID;
ALTER TABLE oidc_config ALTER COLUMN scopes DROP DEFAULT;
ALTER TABLE oidc_config ALTER COLUMN scopes TYPE JSONB USING to_jsonb(string_to_array(scopes, ','));
ALTER TABLE oidc_config ALTER COLUMN scopes SET DEFAULT '["openid","email","profile"]'::jsonb;
