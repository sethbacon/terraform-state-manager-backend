-- Drops TSM's per-app group-mapping table (terraform-suite-identity#206).
--
-- Safe while nothing reads it. Every row is DERIVED -- either dual-written
-- alongside the authoritative write to `sso_settings.oidc_group_mappings`, or
-- reconciled from it at startup -- so dropping the table removes no authority
-- and changes no login-time group resolution.
--
-- That stops being true the moment the read cutover ships: from then on this
-- IS where group mappings come from and this file destroys them. Roll the
-- binary back first.
--
-- ROLLBACK ORDER: this file must run BEFORE 000032's down. group_mappings
-- carries a real foreign key to role_templates, so a rollback that reaches
-- 000032 with this table still standing refuses with 2BP01 -- on purpose:
-- better a loud out-of-order refusal than a CASCADE that silently takes this
-- table with it. golang-migrate's ordinary `down` already runs them in this
-- order; only a by-hand rollback can get it wrong.
DROP INDEX IF EXISTS idx_group_mappings_role_template;
DROP TABLE IF EXISTS group_mappings;
