-- Add GitHub App auth for CI sources, alongside the Entra app auth from 000019.
-- A GitHub app source (auth_method='app', provider='github_actions') mints
-- short-lived installation access tokens from an app id + installation id + RSA
-- private key (PEM, encrypted). The shared auth_method column already exists.
ALTER TABLE ci_sources ADD COLUMN github_app_id TEXT;              -- GitHub App id
ALTER TABLE ci_sources ADD COLUMN github_installation_id TEXT;     -- installation id
ALTER TABLE ci_sources ADD COLUMN encrypted_app_private_key BYTEA; -- AES-256-GCM (PEM)

-- Extend the auth-shape CHECK so an 'app' source is valid with EITHER the Entra
-- triple (Azure DevOps) OR the GitHub App triple, keyed on provider.
ALTER TABLE ci_sources DROP CONSTRAINT ci_sources_auth_shape;
ALTER TABLE ci_sources ADD CONSTRAINT ci_sources_auth_shape CHECK (
    (auth_method = 'pat' AND encrypted_token IS NOT NULL)
 OR (auth_method = 'app' AND provider = 'azure_devops'
     AND tenant_id IS NOT NULL AND client_id IS NOT NULL
     AND encrypted_client_secret IS NOT NULL)
 OR (auth_method = 'app' AND provider = 'github_actions'
     AND github_app_id IS NOT NULL AND github_installation_id IS NOT NULL
     AND encrypted_app_private_key IS NOT NULL)
);
