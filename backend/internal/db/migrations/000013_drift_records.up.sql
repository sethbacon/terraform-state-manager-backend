-- Drift records: one row per currently-known drift condition on a state file —
-- the durable, acknowledgeable signal layered over drift_runs (the mechanism).
-- Re-detections update the existing non-resolved record (counts, summary,
-- last_detected_at, detections) instead of piling up duplicates from nightly
-- scheduled runs; a clean result auto-resolves the record. Records can come
-- from TSM-dispatched runs (origin=run) or from pipelines pushing plan results
-- to /drift/ingest (origin=ingest, deduplicated by external_ref).
CREATE TABLE IF NOT EXISTS drift_records (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id              UUID REFERENCES state_sources(id) ON DELETE SET NULL,
    state_key              TEXT NOT NULL,
    pipeline_connection_id UUID REFERENCES pipeline_connections(id) ON DELETE SET NULL,
    last_run_id            UUID REFERENCES drift_runs(id) ON DELETE SET NULL,
    origin                 TEXT NOT NULL DEFAULT 'run',      -- run | ingest
    severity               TEXT NOT NULL DEFAULT 'warning',  -- critical (resources destroyed) | warning
    added                  INTEGER NOT NULL DEFAULT 0,
    changed                INTEGER NOT NULL DEFAULT 0,
    destroyed              INTEGER NOT NULL DEFAULT 0,
    summary                JSONB NOT NULL DEFAULT '[]'::jsonb, -- [{address, actions}]
    status                 TEXT NOT NULL DEFAULT 'open',     -- open | acknowledged | resolved
    acknowledged_by        TEXT NOT NULL DEFAULT '',
    acknowledged_at        TIMESTAMPTZ,
    ack_note               TEXT NOT NULL DEFAULT '',
    resolved_at            TIMESTAMPTZ,
    external_ref           TEXT,
    detections             INTEGER NOT NULL DEFAULT 1,
    first_detected_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_detected_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one live (non-resolved) record per state: detections collapse onto it.
CREATE UNIQUE INDEX IF NOT EXISTS idx_drift_records_live
    ON drift_records (source_id, state_key) WHERE status <> 'resolved';

-- Ingest idempotency: a pipeline retrying the same external run id must not
-- create a second record.
CREATE UNIQUE INDEX IF NOT EXISTS idx_drift_records_external_ref
    ON drift_records (source_id, external_ref) WHERE external_ref IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_drift_records_status ON drift_records (status, last_detected_at DESC);
