-- Reverse 000020. Restores the 000019 auth-shape CHECK (Entra app only) and
-- drops the GitHub App columns. Fails loudly if any GitHub app source exists
-- (its row would violate the restored CHECK) — convert or delete those first.
ALTER TABLE ci_sources DROP CONSTRAINT ci_sources_auth_shape;
ALTER TABLE ci_sources ADD CONSTRAINT ci_sources_auth_shape CHECK (
    (auth_method = 'pat' AND encrypted_token IS NOT NULL)
 OR (auth_method = 'app' AND tenant_id IS NOT NULL
     AND client_id IS NOT NULL AND encrypted_client_secret IS NOT NULL)
);
ALTER TABLE ci_sources DROP COLUMN IF EXISTS encrypted_app_private_key;
ALTER TABLE ci_sources DROP COLUMN IF EXISTS github_installation_id;
ALTER TABLE ci_sources DROP COLUMN IF EXISTS github_app_id;
