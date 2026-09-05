-- Reverse 000038. Fails loudly (a CHECK violation on the narrowed constraints)
-- if any 'workload_identity' source exists -- convert it to 'app' (or delete
-- it) before rolling back, matching the posture 000019/000020 already
-- established for this same table.
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
ALTER TABLE ci_sources DROP CONSTRAINT ci_sources_auth_method_check;
ALTER TABLE ci_sources ADD CONSTRAINT ci_sources_auth_method_check
    CHECK (auth_method IN ('pat', 'app'));
