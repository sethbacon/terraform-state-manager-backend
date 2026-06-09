-- Repo migration EXECUTE state. Distinct from migration_jobs (state-file
-- storage migration); this tracks Azure DevOps repo/project migration runs and
-- their per-resource checkpoints so an interrupted run can resume.

CREATE TABLE repo_migrations (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id    UUID REFERENCES organizations(id) ON DELETE CASCADE,
    source_org_url     VARCHAR(500) NOT NULL,
    source_project     VARCHAR(255) NOT NULL,
    target_org_url     VARCHAR(500) NOT NULL,
    target_project     VARCHAR(255) NOT NULL,
    status             VARCHAR(50) NOT NULL DEFAULT 'pending',
    total_resources    INT NOT NULL DEFAULT 0,
    created_resources  INT NOT NULL DEFAULT 0,
    skipped_resources  INT NOT NULL DEFAULT 0,
    failed_resources   INT NOT NULL DEFAULT 0,
    started_at         TIMESTAMP,
    completed_at       TIMESTAMP,
    created_by         UUID REFERENCES users(id),
    created_at         TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_repo_migrations_org ON repo_migrations(organization_id);

-- One checkpoint row per resource the execute orchestrator provisions. The
-- unique (migration_id, resource_type, resource_key) triple is what makes a run
-- resumable: on re-run, already-terminal steps are read back and skipped.
CREATE TABLE repo_migration_steps (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    migration_id    UUID NOT NULL REFERENCES repo_migrations(id) ON DELETE CASCADE,
    resource_type   VARCHAR(50) NOT NULL,
    resource_key    VARCHAR(500) NOT NULL,
    status          VARCHAR(50) NOT NULL DEFAULT 'pending',
    detail          TEXT,
    error           TEXT,
    created_at      TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (migration_id, resource_type, resource_key)
);
CREATE INDEX idx_repo_migration_steps_migration ON repo_migration_steps(migration_id);
