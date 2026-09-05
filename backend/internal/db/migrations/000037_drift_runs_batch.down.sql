DROP INDEX IF EXISTS idx_drift_runs_state_created;
DROP INDEX IF EXISTS idx_drift_runs_batch;

ALTER TABLE drift_runs
    DROP COLUMN IF EXISTS ci_run_url,
    DROP COLUMN IF EXISTS ci_run_id,
    DROP COLUMN IF EXISTS batch_id;
