-- 000033_organization_partition (down)
--
-- This one CAN be reversed, and is — unlike the sibling registry's 000056, whose
-- down is a documented no-op because re-adding its constraints would fail on
-- exactly the deployments the up migration had helped. Nothing here is like
-- that: this migration only ADDED nullable columns, a function and some indexes,
-- so removing them restores the previous schema exactly. Reversibility is
-- claimed here only because it is true; the honest part is the list below of
-- what it does NOT restore.
--
-- ============================================================================
-- WHAT THIS DOWN MIGRATION DESTROYS AND CANNOT GIVE BACK
-- ============================================================================
--
-- EVERY TENANCY ASSIGNMENT. DROP COLUMN takes the organization_id values with
-- it. While the column holds nothing but the default organization — which is all
-- Phase 1 ever writes — that is a loss of nothing, because re-running the up
-- migration and letting the startup backfill run reproduces it exactly. It stops
-- being true the moment anything assigns a row to a NON-default organization,
-- whether that is Phase 2's writer or an operator doing it by hand. There is no
-- record of those assignments anywhere else in this database. If any exist, dump
-- them before running this:
--
--   SELECT 'state_sources' AS t, id, organization_id FROM state_sources
--    WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1)
--   UNION ALL SELECT 'pipeline_connections',  id, organization_id FROM pipeline_connections  WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1)
--   UNION ALL SELECT 'ci_sources',            id, organization_id FROM ci_sources            WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1)
--   UNION ALL SELECT 'notification_channels', id, organization_id FROM notification_channels WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1)
--   UNION ALL SELECT 'schedules',             id, organization_id FROM schedules             WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1)
--   UNION ALL SELECT 'state_transfers',       id, organization_id FROM state_transfers       WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1)
--   UNION ALL SELECT 'drift_runs',            id, organization_id FROM drift_runs            WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1)
--   UNION ALL SELECT 'drift_records',         id, organization_id FROM drift_records         WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1)
--   UNION ALL SELECT 'health_runs',           id, organization_id FROM health_runs           WHERE organization_id IS DISTINCT FROM (SELECT default_organization_id FROM system_settings WHERE id = 1);
--
-- An empty result means this rollback is genuinely lossless. A non-empty one
-- means it is not, and the rows it lists are the ones nobody will be able to
-- reconstruct.
--
-- ============================================================================
-- DO NOT RUN THIS ONCE PHASE 3 HAS SHIPPED
-- ============================================================================
--
-- This is the reversal of Phase 1, and a rollback is only safe while the running
-- binary is one that does not READ the column. Phases 2 and 3 change that: Phase
-- 2 reads it behind a flag, Phase 3 reads it unconditionally. Dropping the
-- column out from under a binary that filters on it does not degrade to
-- "everyone sees everything" — it degrades to every query erroring on an unknown
-- column, which is a total outage.
--
-- The correct rollback for a deployment that has moved on is the LATER phase's
-- own lever (Phase 2 and 3 carry a flag precisely so that reads can be turned
-- off without a migration), and only then, with reads back off, this one. The
-- migration cannot check which binary is running, so the check is this
-- paragraph.
--
-- ============================================================================
-- ORDER MATTERS
-- ============================================================================
--
-- The columns go before the function. A column default is a catalogued
-- DEPENDENCY on the function it calls, so DROP FUNCTION while any of the nine
-- defaults still reference it fails with "cannot drop function ... because other
-- objects depend on it" — and golang-migrate marks the version DIRTY on a failed
-- step, which blocks every subsequent up AND down until an operator clears the
-- flag by hand. Dropping the columns removes their defaults, and with them the
-- dependencies, so by the time the function is dropped nothing points at it.
--
-- The indexes are not dropped explicitly: DROP COLUMN drops every index that
-- includes the column, and each of these indexes is on that column alone.

DROP INDEX IF EXISTS idx_state_sources_org;
DROP INDEX IF EXISTS idx_pipeline_connections_org;
DROP INDEX IF EXISTS idx_ci_sources_org;
DROP INDEX IF EXISTS idx_notification_channels_org;
DROP INDEX IF EXISTS idx_schedules_org;
DROP INDEX IF EXISTS idx_state_transfers_org;
DROP INDEX IF EXISTS idx_drift_runs_org;
DROP INDEX IF EXISTS idx_drift_records_org;
DROP INDEX IF EXISTS idx_health_runs_org;

ALTER TABLE state_sources          DROP COLUMN IF EXISTS organization_id;
ALTER TABLE pipeline_connections   DROP COLUMN IF EXISTS organization_id;
ALTER TABLE ci_sources             DROP COLUMN IF EXISTS organization_id;
ALTER TABLE notification_channels  DROP COLUMN IF EXISTS organization_id;
ALTER TABLE schedules              DROP COLUMN IF EXISTS organization_id;
ALTER TABLE state_transfers        DROP COLUMN IF EXISTS organization_id;
ALTER TABLE drift_runs             DROP COLUMN IF EXISTS organization_id;
ALTER TABLE drift_records          DROP COLUMN IF EXISTS organization_id;
ALTER TABLE health_runs            DROP COLUMN IF EXISTS organization_id;

DROP FUNCTION IF EXISTS tsm_default_organization_id();

-- Last, and only now that nothing defaults from it.
ALTER TABLE system_settings DROP COLUMN IF EXISTS default_organization_id;
