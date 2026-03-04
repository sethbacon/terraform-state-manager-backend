CREATE TABLE retention_policies (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name             VARCHAR(255) NOT NULL,
    max_age_days     INT,
    max_count        INT,
    is_default       BOOLEAN DEFAULT false,
    created_at       TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE state_backups (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id   UUID REFERENCES organizations(id) ON DELETE CASCADE,
    source_id         UUID REFERENCES state_sources(id) ON DELETE SET NULL,
    workspace_name    VARCHAR(255) NOT NULL,
    workspace_id      VARCHAR(255),
    storage_backend   VARCHAR(50) NOT NULL,
    storage_path      TEXT NOT NULL,
    file_size_bytes   BIGINT,
    terraform_version VARCHAR(50),
    state_serial      INT,
    checksum_sha256   VARCHAR(64),
    retention_policy_id UUID REFERENCES retention_policies(id),
    expires_at        TIMESTAMP,
    created_at        TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_backups_org ON state_backups(organization_id);
CREATE INDEX idx_backups_expires ON state_backups(expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX idx_backups_workspace ON state_backups(workspace_name);
