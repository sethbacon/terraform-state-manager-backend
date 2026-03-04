CREATE TABLE scheduled_tasks (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name             VARCHAR(255) NOT NULL,
    task_type        VARCHAR(50) NOT NULL,
    schedule         VARCHAR(100) NOT NULL,
    config           JSONB DEFAULT '{}',
    is_active        BOOLEAN DEFAULT true,
    last_run_at      TIMESTAMP,
    next_run_at      TIMESTAMP,
    last_run_status  VARCHAR(50),
    created_by       UUID REFERENCES users(id),
    created_at       TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_scheduled_tasks_org ON scheduled_tasks(organization_id);
CREATE INDEX idx_scheduled_tasks_next_run ON scheduled_tasks(next_run_at) WHERE is_active = true;
