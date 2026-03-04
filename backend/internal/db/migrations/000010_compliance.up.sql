CREATE TABLE compliance_policies (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name             VARCHAR(255) NOT NULL,
    policy_type      VARCHAR(50) NOT NULL,
    config           JSONB NOT NULL,
    severity         VARCHAR(20) DEFAULT 'warning',
    is_active        BOOLEAN DEFAULT true,
    created_at       TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE compliance_results (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id        UUID REFERENCES compliance_policies(id) ON DELETE CASCADE,
    run_id           UUID REFERENCES analysis_runs(id) ON DELETE CASCADE,
    workspace_name   VARCHAR(255) NOT NULL,
    status           VARCHAR(50) NOT NULL,
    violations       JSONB DEFAULT '[]',
    created_at       TIMESTAMP NOT NULL DEFAULT NOW()
);
