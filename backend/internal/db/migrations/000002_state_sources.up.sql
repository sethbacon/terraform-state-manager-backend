CREATE TABLE state_sources (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name             VARCHAR(255) NOT NULL,
    source_type      VARCHAR(50) NOT NULL,
    config           JSONB NOT NULL DEFAULT '{}',
    is_active        BOOLEAN DEFAULT true,
    last_tested_at   TIMESTAMP,
    last_test_status VARCHAR(50),
    created_by       UUID REFERENCES users(id),
    created_at       TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (organization_id, name)
);
CREATE INDEX idx_state_sources_org ON state_sources(organization_id);
CREATE INDEX idx_state_sources_type ON state_sources(source_type);
