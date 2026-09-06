ALTER TABLE drift_records
    DROP COLUMN IF EXISTS drift_summary,
    DROP COLUMN IF EXISTS drift_destroyed,
    DROP COLUMN IF EXISTS drift_changed,
    DROP COLUMN IF EXISTS drift_added;

ALTER TABLE drift_runs
    DROP COLUMN IF EXISTS drift_summary,
    DROP COLUMN IF EXISTS drift_destroyed,
    DROP COLUMN IF EXISTS drift_changed,
    DROP COLUMN IF EXISTS drift_added;
