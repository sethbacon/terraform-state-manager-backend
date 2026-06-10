-- Single-row overlay for the OIDC group-mapping settings that admins can edit at
-- runtime (group claim name, group->org/role mappings, default role). When the
-- row exists its values take precedence over the file config at login; the OIDC
-- provider itself (issuer, client) stays file-configured.
CREATE TABLE IF NOT EXISTS sso_settings (
    id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    oidc_group_claim_name TEXT NOT NULL DEFAULT '',
    oidc_default_role TEXT NOT NULL DEFAULT '',
    oidc_group_mappings JSONB NOT NULL DEFAULT '[]'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
