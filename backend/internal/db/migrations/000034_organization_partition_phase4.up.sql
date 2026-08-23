-- Phase 4 of #393: make organization_id NOT NULL, and re-key the five global
-- unique names to per-organization ones.
--
-- ===========================================================================
-- THIS MIGRATION IS THE POINT OF NO RETURN, AND IT APPLIES AT BOOT.
--
-- It does not decide who owns anything. It RATIFIES whatever ownership exists at
-- the instant it runs, and then makes that permanent: NOT NULL removes the state
-- the boot backfill uses to repair a row, and the re-keyed unique indexes change
-- what names are legal.
--
-- So the precondition is not a property this file can check. It is that the
-- operator has already run `reown-roots verify`, read the distribution, and run
-- `reown-roots move` if the rows are not where they belong. Applying this before
-- that freezes every row in whichever organization it happens to sit in --
-- which, for an estate that predates the acting-organization stamp, is the
-- deployment's default one.
--
-- WHY THERE IS NO AUTOMATED GUARD FOR THAT. The un-re-owned state looks exactly
-- like a legitimate one: "every row is owned by the default organization, and a
-- second organization exists" is both the estate nobody has moved yet AND the
-- estate of a deployment whose second organization is simply new and empty. No
-- predicate distinguishes them, and one that guessed would either block a
-- correct deployment or wave through a wrong one. docs/organization-reown.md
-- carries the sequence instead.
--
-- What this file DOES fail on is a NULL, which is the one unsafe state it can
-- see: SET NOT NULL raises rather than repairing, so a partition root with an
-- unstamped row stops the deployment instead of silently freezing a row that no
-- tenant can see.
-- ===========================================================================

-- ---------------------------------------------------------------------------
-- 1. NOT NULL on all nine partition roots.
--
-- The column DEFAULT stays. It is what keeps a write path that has not been
-- taught the acting organization from failing outright, and after Phase 3 every
-- such path is a bug rather than a design -- but a NOT NULL with no default
-- turns that bug into an outage, and the class guard in
-- internal/db/repositories already fails the build on an unstamped INSERT.
-- ---------------------------------------------------------------------------
ALTER TABLE state_sources         ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE pipeline_connections  ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE ci_sources            ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE schedules             ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE notification_channels ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE state_transfers       ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE drift_runs            ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE drift_records         ALTER COLUMN organization_id SET NOT NULL;
ALTER TABLE health_runs           ALTER COLUMN organization_id SET NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. Re-key the five global unique names.
--
-- FIVE, and the fifth is the one 000033's prose forgot. That comment named
-- state_sources, pipeline_connections, schedules and notification_channels and
-- omitted ci_sources -- which is why
-- TestIntegration_Phase4NameRekeyInventory_IsCompleteAndDerived reads the
-- catalog rather than the migrations. This list was derived the same way.
--
-- The DROP comes first in each pair on purpose: the old index is a strict
-- prefix-free superset of the new constraint, so keeping both would leave the
-- global name unique and make the re-key inert -- a Phase 4 that ran, reported
-- success, and changed nothing about what names are legal.
-- ---------------------------------------------------------------------------
DROP INDEX IF EXISTS idx_state_sources_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_state_sources_org_name
    ON state_sources (organization_id, name);

DROP INDEX IF EXISTS idx_pipeline_connections_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_pipeline_connections_org_name
    ON pipeline_connections (organization_id, name);

DROP INDEX IF EXISTS idx_ci_sources_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_ci_sources_org_name
    ON ci_sources (organization_id, name);

DROP INDEX IF EXISTS idx_schedules_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_org_name
    ON schedules (organization_id, name);

DROP INDEX IF EXISTS idx_notification_channels_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_channels_org_name
    ON notification_channels (organization_id, name);
