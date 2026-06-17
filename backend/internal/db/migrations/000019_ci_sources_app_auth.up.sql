-- Add Microsoft Entra app-registration auth as an alternative to the PAT for CI
-- sources. auth_method selects which credential a source uses:
--   'pat' (default) -> encrypted_token (existing behaviour, unchanged)
--   'app'           -> Entra app registration (tenant_id, client_id, secret),
--                      minted on demand via the client-credentials grant.
-- The PAT column becomes nullable because app sources carry no PAT.
ALTER TABLE ci_sources ADD COLUMN auth_method TEXT NOT NULL DEFAULT 'pat'
    CHECK (auth_method IN ('pat', 'app'));

ALTER TABLE ci_sources ADD COLUMN tenant_id TEXT;                 -- Entra tenant (app)
ALTER TABLE ci_sources ADD COLUMN client_id TEXT;                 -- app registration client id
ALTER TABLE ci_sources ADD COLUMN encrypted_client_secret BYTEA; -- AES-256-GCM (app)

ALTER TABLE ci_sources ALTER COLUMN encrypted_token DROP NOT NULL;

-- Integrity: a source carries exactly the secret its auth_method needs.
ALTER TABLE ci_sources ADD CONSTRAINT ci_sources_auth_shape CHECK (
    (auth_method = 'pat' AND encrypted_token IS NOT NULL)
 OR (auth_method = 'app' AND tenant_id IS NOT NULL
     AND client_id IS NOT NULL AND encrypted_client_secret IS NOT NULL)
);
