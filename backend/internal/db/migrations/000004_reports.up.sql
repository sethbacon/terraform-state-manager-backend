CREATE TABLE reports (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id) ON DELETE CASCADE,
    run_id           UUID REFERENCES analysis_runs(id) ON DELETE SET NULL,
    name             VARCHAR(255) NOT NULL,
    format           VARCHAR(50) NOT NULL,
    storage_path     TEXT NOT NULL,
    file_size_bytes  BIGINT,
    generated_by     UUID REFERENCES users(id),
    created_at       TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_reports_org_id ON reports(organization_id);
