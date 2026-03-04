CREATE TABLE migration_jobs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id   UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name              VARCHAR(255) NOT NULL,
    source_backend    VARCHAR(50) NOT NULL,
    source_config     JSONB NOT NULL,
    target_backend    VARCHAR(50) NOT NULL,
    target_config     JSONB NOT NULL,
    status            VARCHAR(50) NOT NULL DEFAULT 'pending',
    total_files       INT DEFAULT 0,
    migrated_files    INT DEFAULT 0,
    failed_files      INT DEFAULT 0,
    skipped_files     INT DEFAULT 0,
    error_log         JSONB DEFAULT '[]',
    dry_run           BOOLEAN DEFAULT false,
    started_at        TIMESTAMP,
    completed_at      TIMESTAMP,
    created_by        UUID REFERENCES users(id),
    created_at        TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_migration_jobs_org ON migration_jobs(organization_id);
