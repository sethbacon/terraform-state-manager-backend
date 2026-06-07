-- Repo-metadata-derived version pin-drift columns on analysis_results.
-- All nullable with NULL defaults so existing rows and analysis flows that do
-- not supply repo metadata are unaffected.
ALTER TABLE analysis_results
    ADD COLUMN required_version_spec TEXT,
    ADD COLUMN provider_lock_pins    JSONB,
    ADD COLUMN module_constraints    JSONB,
    ADD COLUMN version_drift_report  JSONB;
