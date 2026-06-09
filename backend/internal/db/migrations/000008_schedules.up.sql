-- Scheduler: cron-driven schedules that periodically dispatch a target (initially
-- a drift run on a configured CI pipeline). A background runner polls for due
-- schedules and triggers them, recording the outcome.
CREATE TABLE IF NOT EXISTS schedules (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    cron_expr     TEXT NOT NULL,                       -- cron expression or keyword (daily | weekly | every <dur>)
    target_type   TEXT NOT NULL DEFAULT 'drift',       -- drift (extensible)
    target_config JSONB NOT NULL DEFAULT '{}'::jsonb,  -- e.g. { pipeline_connection_id, source_id, state_key, repo_ref, working_dir }
    enabled       BOOLEAN NOT NULL DEFAULT true,
    last_run_at   TIMESTAMPTZ,
    next_run_at   TIMESTAMPTZ,
    last_run_id   UUID,                                -- the drift_run dispatched on the last fire (no FK: runs may be pruned)
    last_status   TEXT,                                -- success | failed | skipped
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_name ON schedules (name);
-- Partial index for the runner's hot path: enabled schedules ordered by due time.
CREATE INDEX IF NOT EXISTS idx_schedules_due ON schedules (next_run_at) WHERE enabled;
