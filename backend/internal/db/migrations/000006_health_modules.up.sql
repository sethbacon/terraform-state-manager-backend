-- Version lab: also pin module versions.
ALTER TABLE health_runs ADD COLUMN IF NOT EXISTS module_versions JSONB;
