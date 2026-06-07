-- Add drift provenance and an idempotency key to drift_events so the table can
-- hold both snapshot-vs-snapshot drift (the existing background detector) and
-- code drift ingested from an external CI pipeline (Phase 4a).
--
-- drift_source distinguishes the origin of each event:
--   'snapshot' — detected by comparing two state snapshots (existing behaviour)
--   'code'     — ingested from a Terraform plan JSON via POST /api/v1/drift/ingest
--
-- external_ref is a caller-supplied idempotency key (e.g. an ADO pipeline run ID)
-- used to deduplicate repeated ingest requests. It is nullable because
-- snapshot-sourced events have no external reference.

ALTER TABLE drift_events ADD COLUMN drift_source TEXT NOT NULL DEFAULT 'snapshot';
ALTER TABLE drift_events ADD COLUMN external_ref TEXT;

CREATE INDEX idx_drift_events_external_ref ON drift_events(external_ref);
