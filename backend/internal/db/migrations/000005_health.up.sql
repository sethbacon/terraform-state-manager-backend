-- Phase 4 version lab: health runs (plan against pinned versions).
CREATE TABLE IF NOT EXISTS health_runs (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pipeline_connection_id UUID REFERENCES pipeline_connections(id) ON DELETE SET NULL,
    repo_ref               TEXT,
    working_dir            TEXT,
    terraform_version      TEXT,
    provider_versions      JSONB,
    registry_host          TEXT,
    status                 TEXT NOT NULL, -- dispatched | running | completed | failed
    init_ok                BOOLEAN,
    plan_ok                BOOLEAN,
    success                BOOLEAN,
    summary                JSONB,
    detail                 TEXT,
    callback_token         TEXT NOT NULL,
    actor                  TEXT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_health_runs_created ON health_runs (created_at DESC);
