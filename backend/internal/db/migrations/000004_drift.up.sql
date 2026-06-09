-- Phase 3 drift: CI pipeline connections and drift runs.

-- pipeline_connections holds a CI integration (GitHub Actions / Azure DevOps):
-- where to dispatch, plus an encrypted token. Non-secret settings live in config.
CREATE TABLE IF NOT EXISTS pipeline_connections (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    provider        TEXT NOT NULL, -- github_actions | azure_devops
    config          JSONB NOT NULL DEFAULT '{}'::jsonb,
    encrypted_token BYTEA,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_pipeline_connections_name ON pipeline_connections (name);

-- drift_runs tracks a dispatched terraform-plan run and the drift it found.
-- The CI job authenticates its result POST with the per-run callback_token.
CREATE TABLE IF NOT EXISTS drift_runs (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pipeline_connection_id UUID REFERENCES pipeline_connections(id) ON DELETE SET NULL,
    source_id              UUID REFERENCES state_sources(id) ON DELETE SET NULL,
    state_key              TEXT,
    repo_ref               TEXT,
    working_dir            TEXT,
    status                 TEXT NOT NULL, -- dispatched | running | completed | failed
    added                  INTEGER,
    changed                INTEGER,
    destroyed              INTEGER,
    drifted                BOOLEAN,
    summary                JSONB,
    detail                 TEXT,
    callback_token         TEXT NOT NULL,
    actor                  TEXT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_drift_runs_created ON drift_runs (created_at DESC);
