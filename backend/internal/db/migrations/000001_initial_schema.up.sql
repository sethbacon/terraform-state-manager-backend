-- Initial schema migration for terraform-state-manager backend

-- Organizations
CREATE TABLE organizations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) UNIQUE NOT NULL,
    display_name VARCHAR(255),
    description TEXT,
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Users
CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email VARCHAR(255) UNIQUE NOT NULL,
    name VARCHAR(255) NOT NULL,
    oidc_sub VARCHAR(255) UNIQUE,
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Role templates
CREATE TABLE role_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(100) UNIQUE NOT NULL,
    display_name VARCHAR(255),
    description TEXT,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    is_system BOOLEAN DEFAULT false,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Organization members (join table between organizations and users with role)
CREATE TABLE organization_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_template_id UUID REFERENCES role_templates(id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id, user_id)
);

-- API keys
CREATE TABLE api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    key_hash VARCHAR(255) NOT NULL,
    key_prefix VARCHAR(20) NOT NULL,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    expires_at TIMESTAMP,
    last_used_at TIMESTAMP,
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_api_keys_key_prefix ON api_keys(key_prefix);

-- Audit logs
CREATE TABLE audit_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    organization_id UUID REFERENCES organizations(id) ON DELETE SET NULL,
    action VARCHAR(500) NOT NULL,
    resource_type VARCHAR(100),
    resource_id VARCHAR(255),
    ip_address VARCHAR(45),
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_audit_logs_created_at ON audit_logs(created_at DESC);
CREATE INDEX idx_audit_logs_user_id ON audit_logs(user_id);

-- OIDC configuration
CREATE TABLE oidc_config (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issuer_url TEXT NOT NULL,
    client_id VARCHAR(255) NOT NULL,
    client_secret_encrypted TEXT NOT NULL,
    redirect_url TEXT NOT NULL,
    scopes TEXT DEFAULT 'openid,email,profile',
    is_active BOOLEAN DEFAULT true,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- System settings (key-value store)
CREATE TABLE system_settings (
    key VARCHAR(255) PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Insert default role templates
INSERT INTO role_templates (name, display_name, description, scopes, is_system) VALUES
    ('admin', 'Administrator', 'Full administrative access', ARRAY['admin', 'analysis:read', 'analysis:write', 'reports:read', 'reports:write', 'dashboard:read', 'dashboard:write', 'sources:read', 'sources:write', 'compliance:read', 'compliance:write', 'users:read', 'users:write', 'organizations:read', 'organizations:write', 'api_keys:read', 'api_keys:write', 'audit:read', 'settings:read', 'settings:write'], true),
    ('analyst', 'Analyst', 'Analysis and reporting access', ARRAY['analysis:read', 'analysis:write', 'reports:read', 'reports:write', 'dashboard:read', 'sources:read', 'compliance:read'], true),
    ('viewer', 'Viewer', 'Read-only access', ARRAY['analysis:read', 'reports:read', 'dashboard:read', 'sources:read'], true),
    ('operator', 'Operator', 'Operational access without administration', ARRAY['analysis:read', 'analysis:write', 'reports:read', 'reports:write', 'dashboard:read', 'dashboard:write', 'sources:read', 'sources:write', 'compliance:read', 'compliance:write', 'users:read', 'organizations:read', 'api_keys:read', 'api_keys:write', 'audit:read', 'settings:read'], true);

-- Insert default organization
INSERT INTO organizations (name, display_name, description) VALUES
    ('default', 'Default Organization', 'Default organization for all users');

-- Insert default system settings
INSERT INTO system_settings (key, value) VALUES
    ('setup_completed', 'false');
