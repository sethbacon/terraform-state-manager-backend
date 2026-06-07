-- Reverse 000013_drift_source: drop the idempotency index and the two columns.

DROP INDEX IF EXISTS idx_drift_events_external_ref;

ALTER TABLE drift_events DROP COLUMN IF EXISTS external_ref;
ALTER TABLE drift_events DROP COLUMN IF EXISTS drift_source;
