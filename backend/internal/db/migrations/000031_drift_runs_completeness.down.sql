ALTER TABLE drift_runs
    DROP COLUMN IF EXISTS truncated,
    DROP COLUMN IF EXISTS omitted_entries,
    DROP COLUMN IF EXISTS omitted_attrs,
    DROP COLUMN IF EXISTS unparseable,
    DROP COLUMN IF EXISTS unmasked;
