-- Runtime OIDC configuration set by the setup wizard, in TSM's app/domain DB so
-- a standalone TSM owns it without writing the shared identity schema. The
-- client secret is encrypted at rest (AES-256-GCM via internal/crypto), so it is
-- stored as raw bytes. At most one row is active; the active config is loaded
-- into the live auth handler at boot and on save (no restart needed).
CREATE TABLE IF NOT EXISTS oidc_configs (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    issuer_url              TEXT        NOT NULL,
    client_id               TEXT        NOT NULL,
    client_secret_encrypted BYTEA       NOT NULL,
    redirect_url            TEXT        NOT NULL,
    scopes                  JSONB       NOT NULL DEFAULT '["openid","email","profile"]'::jsonb,
    is_active               BOOLEAN     NOT NULL DEFAULT true,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only one active OIDC config at a time.
CREATE UNIQUE INDEX IF NOT EXISTS idx_oidc_configs_active ON oidc_configs (is_active) WHERE is_active = true;
