-- Select the compliance evaluation engine per policy. Existing policies default
-- to the built-in custom rules engine; setting 'opa' routes evaluation through
-- the embedded OPA/Rego engine instead.
ALTER TABLE compliance_policies
    ADD COLUMN engine_type TEXT NOT NULL DEFAULT 'custom';
