-- CI sources: org-level CI provider connections (an Azure DevOps org/project or
-- a GitHub owner) with a credential stored once, so pipeline connections can be
-- created by picking a discovered pipeline/repo/workflow instead of hand-typing
-- coordinates and re-entering tokens (mirrors the registry's SCM-provider model).
CREATE TABLE IF NOT EXISTS ci_sources (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    provider        TEXT NOT NULL, -- github_actions | azure_devops
    organization    TEXT NOT NULL, -- ADO organization / GitHub owner (org or user)
    project         TEXT,          -- ADO project (NULL for GitHub)
    encrypted_token BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ci_sources_name ON ci_sources (name);
