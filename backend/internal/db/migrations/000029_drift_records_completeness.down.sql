DROP INDEX IF EXISTS idx_drift_records_incomplete;

ALTER TABLE drift_records
    DROP COLUMN IF EXISTS truncated,
    DROP COLUMN IF EXISTS omitted_entries,
    DROP COLUMN IF EXISTS omitted_attrs,
    DROP COLUMN IF EXISTS unparseable,
    DROP COLUMN IF EXISTS unmasked;
