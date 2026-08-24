-- Reverse Phase 4.
--
-- THIS RESTORES THE SHAPE, NOT THE ESTATE. Dropping NOT NULL cannot un-freeze
-- ownership, and restoring a GLOBAL unique name can FAIL where the up-migration
-- has already allowed two organizations to use the same one -- which is the
-- point of the whole phase, so it is expected rather than a fault. An operator
-- rolling back a deployment that has since created colliding names has to
-- resolve them by hand first; there is no answer DDL can pick.

DROP INDEX IF EXISTS idx_state_sources_org_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_state_sources_name ON state_sources (name);

DROP INDEX IF EXISTS idx_pipeline_connections_org_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_pipeline_connections_name ON pipeline_connections (name);

DROP INDEX IF EXISTS idx_ci_sources_org_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_ci_sources_name ON ci_sources (name);

DROP INDEX IF EXISTS idx_schedules_org_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_name ON schedules (name);

DROP INDEX IF EXISTS idx_notification_channels_org_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_channels_name ON notification_channels (name);

ALTER TABLE state_sources         ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE pipeline_connections  ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE ci_sources            ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE schedules             ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE notification_channels ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE state_transfers       ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE drift_runs            ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE drift_records         ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE health_runs           ALTER COLUMN organization_id DROP NOT NULL;
