-- 000031_app_role_authorization
--
-- TSM's OWN role definitions and role assignments, in TSM's own schema.
-- Refs sethbacon/terraform-suite-identity#206 (Phase 3a, state-manager side).
--
-- WHAT THIS ADDS THAT DID NOT EXIST. TSM owns no identity tables: it consumes
-- the shared library's repositories directly, so today "the viewer role" and
-- "alice is an editor of acme" are rows in identity.role_templates and
-- identity.organization_members — SHARED state. identity.role_templates.name is
-- globally UNIQUE, so TSM and the registry seed the same six names into the same
-- six rows and overwrite each other's scopes on every restart. That is the whole
-- reason TSM_SUITE_ROLE_SEED_OWNER (self|registry|tsm) exists. Under the agreed
-- model identity is shared and AUTHORIZATION IS PER-APP: membership stays a fact
-- in identity, and which role that member holds HERE is a row here.
--
-- NOTHING OBSERVABLE CHANGES IN THIS MIGRATION. Reads still come from
-- identity.organization_members joined to identity.role_templates, exactly as
-- before. These two tables are written in lockstep with those (internal/approles)
-- and reconciled from them at every startup, so that the phase which switches
-- reads has something correct to switch to. Switching reads is a later phase.
--
-- NO FOREIGN KEYS TO IDENTITY, DELIBERATELY. organization_id and user_id name
-- identity.organizations and identity.users rows and carry no constraint,
-- because identity may live in this database's `identity` schema OR in a
-- separate database entirely (TSM_IDENTITY_DATABASE_*), where Postgres cannot
-- express a foreign key at all. This migration runs on the APP connection; the
-- identity connection is a different pool. Registry's 000046 and 000051 and this
-- repo's own 000030 reached the same conclusion for the same reason. The FK to
-- role_templates below IS expressible, because that table is ours, and it is
-- taken: a role assignment pointing at a role that does not exist is not a
-- tenancy question, it is a corrupt authorization record.
--
-- ON DELETE SET NULL mirrors identity.organization_members.role_template_id
-- exactly (identity/migrations/000001). A membership with a NULL role is
-- already representable there — AddMemberWithRoleTemplate(nil) is a public API
-- of the shared store — so the mirror must be able to represent it too, and
-- dropping a template must degrade an assignment the same way on both sides
-- rather than either erroring or deleting the membership.

-- ===========================================================================
-- ROUTING PRE-CHECK
--
-- These tables are created UNQUALIFIED, so the app connection's search_path
-- places them — the same routing platform_admins and audit_outbox already use
-- (000030). That is fine, and correct, right up until the search_path bridges
-- the boundary this phase exists to draw: with `search_path=identity,public`,
-- `CREATE TABLE IF NOT EXISTS role_templates` finds identity's table, creates
-- nothing, reports success, and every write below lands in the SHARED table
-- these tables were added to stop sharing. The collision would be reintroduced
-- silently, by a migration that appeared to have run.
--
-- `organization_members` is the discriminator, and a general one: it is a name
-- identity owns and TSM never creates, so its visibility from THIS connection
-- means precisely "identity's tables are unqualified-reachable here". No schema
-- name is hard-coded, so a deployment that renames the identity schema is
-- checked just the same.
--
-- REFUSING IS THE POINT. An operator whose app connection is routed into
-- identity must fix the routing (unset the app pool's search_path override;
-- TSM's app connection takes the server default, and only the IDENTITY pool is
-- given `options='-c search_path=identity,public'`), not discover months later
-- that this deployment's authorization was never separated at all.
-- ===========================================================================
DO $$
BEGIN
    IF to_regclass('organization_members') IS NOT NULL THEN
        RAISE EXCEPTION 'app connection resolves identity''s organization_members unqualified (search_path=%): TSM''s per-app authorization tables would be created in, or shadowed by, the shared identity schema',
            current_setting('search_path')
            USING ERRCODE = '42P07',
                  HINT = 'Remove the identity search_path override from the APPLICATION database connection; only the identity pool may carry it.';
    END IF;
END $$;

-- ===========================================================================
-- role_templates — TSM's own role -> scope mapping.
--
-- Column-for-column the shape of identity.role_templates as it stands after the
-- library's 000003 (scopes is JSONB there, not the TEXT[] of 000001), because
-- the backfill copies those rows verbatim and Phase 3b's reader is the same
-- reader. A divergent shape here would make the two phases' comparisons a
-- translation exercise instead of an equality.
--
-- id IS NOT REGENERATED BY THE BACKFILL. It carries identity's own uuid for
-- every row that already exists there, so organization_member_roles can store
-- the SAME role_template_id the identity membership row stores. That is what
-- makes the mirror a direct copy rather than a name-mediated translation, makes
-- the drift query below a row comparison, and lets an operator diff the two
-- tables by primary key. A uuid is a name, not a reference; sharing its value
-- across a boundary that carries no foreign key costs nothing and survives
-- identity.role_templates being dropped in Phase 4.
--
-- name is UNIQUE here too, and now that is a per-app uniqueness — which is the
-- entire point. Two apps can each have their own `admin` with their own scopes.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS role_templates (
    id           UUID         PRIMARY KEY,
    name         VARCHAR(100) NOT NULL UNIQUE,
    display_name VARCHAR(255),
    description  TEXT,
    scopes       JSONB        NOT NULL DEFAULT '[]'::jsonb,
    is_system    BOOLEAN      NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- ===========================================================================
-- organization_member_roles — (organization_id, user_id) -> role_template_id.
--
-- The primary key is the pair, matching identity.organization_members'
-- UNIQUE(organization_id, user_id): a member holds at most one role per
-- organization, and the mirror's upsert needs a conflict target that says so.
-- There is no surrogate id — nothing references a row of this table, and a
-- surrogate would let two rows for one pair exist long enough to be read.
--
-- mirrored_at is NOT decoration. The startup reconcile stamps it on every row it
-- writes and then deletes everything older than the generation it started with,
-- which is how assignments that disappeared from identity WITHOUT passing
-- through this app's code — an organization or user deleted by CASCADE, a row
-- removed by the sibling registry, a mirror write that failed after its identity
-- leg committed — stop being represented here. Without it the sweep would have
-- to hold every membership in memory to compute the difference.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS organization_member_roles (
    organization_id  UUID        NOT NULL,
    user_id          UUID        NOT NULL,
    role_template_id UUID        REFERENCES role_templates(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    mirrored_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, user_id)
);

-- "What does this principal hold across the deployment?" — the union query
-- Phase 3b's session-scope derivation runs on every login. The primary key
-- leads with organization_id, so it cannot serve this.
CREATE INDEX IF NOT EXISTS organization_member_roles_user_idx
    ON organization_member_roles (user_id);

-- The reconcile sweep's scan, and the drift query's.
CREATE INDEX IF NOT EXISTS organization_member_roles_mirrored_at_idx
    ON organization_member_roles (mirrored_at);

-- ===========================================================================
-- ROUTING POST-CHECK
--
-- The pre-check above proves identity is not reachable unqualified. This proves
-- the positive: the two names this app will write to resolve to the schema the
-- CREATEs just targeted, and not to some third schema earlier in the path. Both
-- halves are needed — the pre-check catches `identity,public`, this catches a
-- pre-existing shadow in any other schema ahead of ours.
-- ===========================================================================
DO $$
DECLARE
    resolved TEXT;
    tbl      TEXT;
BEGIN
    FOREACH tbl IN ARRAY ARRAY['role_templates', 'organization_member_roles'] LOOP
        SELECT n.nspname INTO resolved
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE c.oid = to_regclass(tbl);
        IF resolved IS DISTINCT FROM current_schema() THEN
            RAISE EXCEPTION 'unqualified % resolves to schema % but was created in %: TSM would read and write two different tables',
                tbl, coalesce(resolved, '<nothing>'), current_schema()
                USING ERRCODE = '42P07';
        END IF;
    END LOOP;
END $$;
