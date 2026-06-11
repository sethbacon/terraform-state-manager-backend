-- Persistent per-state analysis store: the dashboard aggregates these rows
-- instead of re-reading every state file from its backend on each request.
-- Rows are reconciled by the statesync service, which diffs connector listings
-- against version_marker (size + last-modified / workspace updated-at) and only
-- re-reads states that changed.
CREATE TABLE IF NOT EXISTS state_analyses (
    source_id         UUID NOT NULL REFERENCES state_sources(id) ON DELETE CASCADE,
    state_key         TEXT NOT NULL,
    version_marker    TEXT NOT NULL DEFAULT '',
    size              BIGINT NOT NULL DEFAULT 0,
    terraform_version TEXT NOT NULL DEFAULT '',
    serial            BIGINT NOT NULL DEFAULT 0,
    lineage           TEXT NOT NULL DEFAULT '',
    rum               INTEGER NOT NULL DEFAULT 0,
    managed_resources INTEGER NOT NULL DEFAULT 0,
    data_sources      INTEGER NOT NULL DEFAULT 0,
    total_resources   INTEGER NOT NULL DEFAULT 0,
    providers         JSONB NOT NULL DEFAULT '{}'::jsonb,
    resource_types    JSONB NOT NULL DEFAULT '{}'::jsonb,
    analyzed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, state_key)
);

-- One row per source: outcome of its most recent sync cycle.
CREATE TABLE IF NOT EXISTS source_sync_status (
    source_id     UUID PRIMARY KEY REFERENCES state_sources(id) ON DELETE CASCADE,
    last_sync_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    states_listed INTEGER NOT NULL DEFAULT 0,
    read_errors   INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT ''
);
