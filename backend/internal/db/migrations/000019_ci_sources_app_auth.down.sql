-- Reverse 000019. Fails loudly (NOT NULL violation) if any 'app' source exists,
-- because those rows have no encrypted_token to restore the NOT NULL constraint
-- on — convert or delete app sources before rolling back.
ALTER TABLE ci_sources DROP CONSTRAINT IF EXISTS ci_sources_auth_shape;
ALTER TABLE ci_sources ALTER COLUMN encrypted_token SET NOT NULL;
ALTER TABLE ci_sources DROP COLUMN IF EXISTS encrypted_client_secret;
ALTER TABLE ci_sources DROP COLUMN IF EXISTS client_id;
ALTER TABLE ci_sources DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE ci_sources DROP COLUMN IF EXISTS auth_method;
