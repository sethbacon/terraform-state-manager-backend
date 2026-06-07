-- Reverse 000014_analysis_repo_metadata: drop the repo-metadata drift columns.
ALTER TABLE analysis_results DROP COLUMN IF EXISTS version_drift_report;
ALTER TABLE analysis_results DROP COLUMN IF EXISTS module_constraints;
ALTER TABLE analysis_results DROP COLUMN IF EXISTS provider_lock_pins;
ALTER TABLE analysis_results DROP COLUMN IF EXISTS required_version_spec;
