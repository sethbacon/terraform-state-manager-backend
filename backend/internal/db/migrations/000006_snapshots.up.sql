CREATE TABLE state_snapshots (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id) ON DELETE CASCADE,
    source_id        UUID REFERENCES state_sources(id) ON DELETE SET NULL,
    workspace_name   VARCHAR(255) NOT NULL,
    workspace_id     VARCHAR(255),
    snapshot_data    JSONB NOT NULL,
    resource_count   INT DEFAULT 0,
    rum_count        INT DEFAULT 0,
    terraform_version VARCHAR(50),
    state_serial     INT,
    captured_at      TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_snapshots_workspace_time ON state_snapshots(workspace_name, captured_at DESC);
CREATE INDEX idx_snapshots_org ON state_snapshots(organization_id);

CREATE TABLE drift_events (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id),
    workspace_name   VARCHAR(255) NOT NULL,
    snapshot_before  UUID REFERENCES state_snapshots(id),
    snapshot_after   UUID REFERENCES state_snapshots(id),
    changes          JSONB NOT NULL,
    severity         VARCHAR(20) NOT NULL DEFAULT 'info',
    detected_at      TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_drift_events_org ON drift_events(organization_id);
CREATE INDEX idx_drift_events_workspace ON drift_events(workspace_name);
