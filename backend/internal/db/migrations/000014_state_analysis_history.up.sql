-- Append-only history of state analyses: one row per OBSERVED CHANGE of a
-- state (statesync appends only when the analysis differs from the latest
-- history row, so marker-less backends that are re-read every cycle don't
-- pile up duplicates). Powers per-state time series (RUM/resource growth,
-- terraform-version adoption) and point-in-time diffs. Pruned after 180 days.
CREATE TABLE IF NOT EXISTS state_analysis_history (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
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
    analyzed_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_state_analysis_history_state
    ON state_analysis_history (source_id, state_key, analyzed_at DESC);
