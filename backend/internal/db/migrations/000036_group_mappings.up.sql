-- 000036_group_mappings
--
-- TSM's own per-app group-mapping table (sethbacon/terraform-suite-identity#206,
-- phase 2: "each app creates its per-app tables and backfills"; state-manager
-- side, following 000032's role tables and the registry sibling's 000059).
--
-- The target model is: identity is SHARED, authorization is PER-APP. An IdP
-- group mapping is authorization policy -- "members of IdP group G get THIS
-- APP's role R in organization O" -- so #206 places it in the app's own schema
-- as `<app>.group_mappings`, next to `role_templates` and
-- `organization_member_roles` (migration 000032).
--
-- THIS MIGRATION CHANGES NOTHING OBSERVABLE. The DB-stored group mappings are
-- read today from the `oidc_group_mappings` JSONB list in the single
-- `sso_settings` overlay row (migration 000010), with the file config as the
-- fallback when no overlay row exists (effectiveOIDCGroupConfig in
-- internal/api/auth.go). Every read keeps coming from there. The application
-- dual-writes the mapping rows below so that the read cutover, which is a
-- separate change, has a populated and continuously reconciled copy to switch
-- onto.
--
--
-- WHERE GROUP MAPPINGS LIVE TODAY, PRECISELY
--
-- Unlike the registry -- whose mappings ride each oidc_config row's
-- extra_config on the identity connection -- TSM keeps ONE ordered JSON list,
-- in a singleton row of its own `sso_settings` table, written by exactly one
-- statement (SSOSettingsRepository.Upsert, reached from
-- PUT /admin/oidc/group-mapping). Order is LOAD-BEARING: the shared library's
-- ResolveGroupMappings resolves competing mappings first-match-wins in
-- configuration order (terraform-suite-identity#269; adopted here by #488
-- after this app resolved LAST-match-wins), which is why this table has a
-- `position` column and why it is the primary key.
--
-- The SAML, LDAP and mTLS mappings are file configuration only -- they have no
-- database write sites, so a dual-write phase has nothing to mirror for them.
-- How they enter this table (probably as seeded rows) is the read cutover's
-- decision, not this migration's.
--
-- `oidc_group_claim_name` and `oidc_default_role` are provider settings, not
-- mapping rows; they stay in `sso_settings` with the rest of the overlay. #206
-- assigns only the group->role rows to the app table.
--
--
-- WHY `group_mappings` AND NOT A PREFIXED NAME
--
-- 000032 took the unprefixed names `role_templates` /
-- `organization_member_roles` because no app-side duplicate existed and the
-- routing pre-check below keeps identity's same-named tables out of reach. No
-- table named `group_mappings` exists in ANY topology -- not in this app's
-- schema, not in the shared identity schema (verified against
-- terraform-suite-identity v0.36.0's migrations) -- so, by the same rule, this
-- table takes the name #206 specifies.
--
--
-- COLUMNS, AND THE ONE REAL FOREIGN KEY
--
-- `position` is the mapping's index in the stored list. Primary key, because
-- the source is one ordered list and first-match-wins hangs on the order.
--
-- `organization_name` names an identity organization BY NAME, because that is
-- what the stored mappings carry (the {group, organization, role} triple) and
-- what the login-time reconciliation resolves. It points across the identity
-- boundary, so per the decided tenancy model it carries NO foreign key -- the
-- same reasoning as 000030 and 000032: identity may be another schema or
-- another database (TSM_IDENTITY_DATABASE_*), where the key cannot be
-- expressed at all.
--
-- `role_template_name` is the faithful copy of the stored string. Kept
-- verbatim because a mapping may legitimately name a template that does not
-- (yet, or any longer) resolve -- today that mapping simply confers nothing at
-- login (the membership write's own name lookup reports it; see
-- guardProvisionableRole's doc) -- and a mirror that dropped or rejected such
-- a row could not be compared 1:1 against its source.
--
-- `role_template_id` is the app-local resolution of that name, and it is the
-- one REAL foreign key here: role_templates is TSM's own table, created by
-- 000032 on this same connection, and what every role and scope actually
-- resolves from since the Phase 3 read switch. NULL means "the name does not
-- currently resolve". ON DELETE SET NULL mirrors what deleting a template does
-- to that mapping's effect at login: the mapping stays configured, it just
-- confers nothing. The dual-write re-resolves the name on every mirror write
-- and the boot reconcile re-resolves it on every boot, so a template created
-- AFTER a mapping that names it converges.
--
--
-- NO BACKFILL HERE -- SAME REASONING AS 000032'S, STATED FOR THIS SOURCE
--
-- The source row lives in `sso_settings`, which the auth handlers read through
-- the IDENTITY pool (search_path identity,public) -- a different connection
-- from the one this migration runs on, and in the split-database topology a
-- different DATABASE. A SQL backfill here could silently capture the wrong
-- side. The backfill is therefore done in Go, at startup, reading through the
-- very connection the application resolves the overlay through
-- (internal/db/repositories.ReconcileGroupMappings), which makes the effective
-- source identical BY CONSTRUCTION. It re-runs on every boot, so a deployment
-- that upgrades before this table exists converges rather than staying
-- half-populated.
--
-- Consequence, stated rather than discovered later: immediately after this
-- migration the table is EMPTY. Nothing reads it yet, so that is not an
-- outage; it is the state the startup reconcile exists to resolve.

-- ===========================================================================
-- ROUTING PRE-CHECK -- same check, same discriminator, same reasoning as
-- 000032. This table is created UNQUALIFIED, so a misrouted app connection
-- (search_path=identity,public) would create it in the identity schema and
-- point its foreign key at identity.role_templates -- re-crossing the exact
-- boundary this phase draws. Refusing is the point.
-- ===========================================================================
DO $$
BEGIN
    IF to_regclass('organization_members') IS NOT NULL THEN
        RAISE EXCEPTION 'app connection resolves identity''s organization_members unqualified (search_path=%): TSM''s per-app group_mappings table would be created in, or referenced against, the shared identity schema',
            current_setting('search_path')
            USING ERRCODE = '42P07',
                  HINT = 'Remove the identity search_path override from the APPLICATION database connection; only the identity pool may carry it.';
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS group_mappings (
    position           INTEGER     NOT NULL CHECK (position >= 0),
    group_name         TEXT        NOT NULL,
    organization_name  TEXT        NOT NULL,
    role_template_name TEXT        NOT NULL,
    role_template_id   UUID        REFERENCES role_templates(id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (position)
);

-- No updated_at: rows are replaced wholesale. A change to the overlay's
-- mapping list deletes and re-inserts every row in one transaction (the list
-- is small and ordered, and positions shift on any edit), so a per-row
-- updated_at could never differ from created_at.
COMMENT ON TABLE group_mappings IS
    'TSM''s own IdP group -> role-template mappings (terraform-suite-identity#206). Mirrors the oidc_group_mappings list in the sso_settings overlay row; written by the dual-write in internal/db/repositories and the boot reconcile; NOT yet read.';

CREATE INDEX IF NOT EXISTS idx_group_mappings_role_template
    ON group_mappings (role_template_id);
