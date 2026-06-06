-- Reverse the canonical identity reconciliation (best-effort). Converts the
-- JSONB scope columns back to their original TEXT[]/TEXT types and drops the
-- columns added by the up migration. The added columns the canonical model uses
-- but the legacy shape lacked are dropped; legacy columns left untouched by the
-- up migration are unaffected.

-- oidc_config: drop registry-shape columns; scopes JSONB -> comma-separated TEXT.
ALTER TABLE oidc_config ALTER COLUMN scopes DROP DEFAULT;
ALTER TABLE oidc_config ALTER COLUMN scopes TYPE TEXT
    USING array_to_string(ARRAY(SELECT jsonb_array_elements_text(scopes)), ',');
ALTER TABLE oidc_config ALTER COLUMN scopes SET DEFAULT 'openid,email,profile';
ALTER TABLE oidc_config DROP COLUMN IF EXISTS updated_by;
ALTER TABLE oidc_config DROP COLUMN IF EXISTS created_by;
ALTER TABLE oidc_config DROP COLUMN IF EXISTS extra_config;
ALTER TABLE oidc_config DROP COLUMN IF EXISTS provider_type;
ALTER TABLE oidc_config DROP COLUMN IF EXISTS name;

-- api_keys: drop expiry-notification; scopes JSONB -> TEXT[].
ALTER TABLE api_keys DROP COLUMN IF EXISTS expiry_notification_sent_at;
ALTER TABLE api_keys ALTER COLUMN scopes DROP DEFAULT;
ALTER TABLE api_keys ALTER COLUMN scopes TYPE TEXT[]
    USING ARRAY(SELECT jsonb_array_elements_text(scopes));
ALTER TABLE api_keys ALTER COLUMN scopes SET DEFAULT '{}';

-- role_templates: scopes JSONB -> TEXT[].
ALTER TABLE role_templates ALTER COLUMN scopes DROP DEFAULT;
ALTER TABLE role_templates ALTER COLUMN scopes TYPE TEXT[]
    USING ARRAY(SELECT jsonb_array_elements_text(scopes));
ALTER TABLE role_templates ALTER COLUMN scopes SET DEFAULT '{}';

-- organizations: drop IdP binding.
ALTER TABLE organizations DROP COLUMN IF EXISTS idp_name;
ALTER TABLE organizations DROP COLUMN IF EXISTS idp_type;
