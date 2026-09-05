-- Add AKS Workload Identity as a third Azure DevOps auth_method for CI sources
-- (drift-fleet-scale.md Phase 1b): TSM's own pod identity, federated to a
-- dedicated user-assigned managed identity, mints Azure DevOps access tokens
-- with NO secret material stored in TSM at all -- not a PAT, not an Entra
-- client secret. Only the managed identity's client_id is non-secret
-- configuration; tenant_id and the federated token file path are resolved from
-- the pod's own environment (set by the AKS workload-identity webhook), so
-- they are deliberately not columns here.
ALTER TABLE ci_sources DROP CONSTRAINT ci_sources_auth_method_check;
ALTER TABLE ci_sources ADD CONSTRAINT ci_sources_auth_method_check
    CHECK (auth_method IN ('pat', 'app', 'workload_identity'));

-- Integrity: a workload_identity source carries client_id and NONE of the
-- other three secret-bearing columns -- not the PAT, not the Entra client
-- secret, not the GitHub App key. Azure DevOps only: Workload Identity mints
-- an Entra token for the fixed ADO resource id, which has no GitHub equivalent.
ALTER TABLE ci_sources DROP CONSTRAINT ci_sources_auth_shape;
ALTER TABLE ci_sources ADD CONSTRAINT ci_sources_auth_shape CHECK (
    (auth_method = 'pat' AND encrypted_token IS NOT NULL)
 OR (auth_method = 'app' AND provider = 'azure_devops'
     AND tenant_id IS NOT NULL AND client_id IS NOT NULL
     AND encrypted_client_secret IS NOT NULL)
 OR (auth_method = 'app' AND provider = 'github_actions'
     AND github_app_id IS NOT NULL AND github_installation_id IS NOT NULL
     AND encrypted_app_private_key IS NOT NULL)
 OR (auth_method = 'workload_identity' AND provider = 'azure_devops'
     AND client_id IS NOT NULL
     AND encrypted_token IS NULL AND encrypted_client_secret IS NULL
     AND encrypted_app_private_key IS NULL)
);
