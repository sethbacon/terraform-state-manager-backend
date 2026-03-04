CREATE TABLE notification_channels (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name             VARCHAR(255) NOT NULL,
    channel_type     VARCHAR(50) NOT NULL,
    config           JSONB NOT NULL,
    is_active        BOOLEAN DEFAULT true,
    created_at       TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE alert_rules (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name             VARCHAR(255) NOT NULL,
    rule_type        VARCHAR(50) NOT NULL,
    config           JSONB NOT NULL,
    severity         VARCHAR(20) DEFAULT 'warning',
    channel_ids      JSONB DEFAULT '[]',
    is_active        BOOLEAN DEFAULT true,
    created_at       TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE alerts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID REFERENCES organizations(id),
    rule_id          UUID REFERENCES alert_rules(id) ON DELETE SET NULL,
    workspace_name   VARCHAR(255),
    severity         VARCHAR(20) NOT NULL,
    title            VARCHAR(500) NOT NULL,
    description      TEXT,
    metadata         JSONB DEFAULT '{}',
    is_acknowledged  BOOLEAN DEFAULT false,
    acknowledged_by  UUID REFERENCES users(id),
    acknowledged_at  TIMESTAMP,
    created_at       TIMESTAMP NOT NULL DEFAULT NOW()
);
