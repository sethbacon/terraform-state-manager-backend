-- First-run setup wizard state. Single-row table (id = 1) in TSM's app/domain
-- DB (NOT the shared identity schema), so a standalone TSM fully owns its setup
-- state and never writes the shared identity store — the no-clobber boundary.
-- setup_token_hash is the bcrypt hash of the one-time setup token (the raw value
-- is printed at boot); it is NULLed by /setup/complete to permanently disable
-- the wizard (a GitOps-safe self-disable driven by DB state, not chart state).
CREATE TABLE IF NOT EXISTS system_settings (
    id                 INTEGER     PRIMARY KEY DEFAULT 1,
    setup_completed    BOOLEAN     NOT NULL DEFAULT false,
    setup_token_hash   TEXT,
    admin_configured   BOOLEAN     NOT NULL DEFAULT false,
    auth_method        TEXT,
    oidc_configured    BOOLEAN     NOT NULL DEFAULT false,
    ldap_configured    BOOLEAN     NOT NULL DEFAULT false,
    sources_configured BOOLEAN     NOT NULL DEFAULT false,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT system_settings_singleton CHECK (id = 1)
);

-- Ensure the singleton row exists so every read/UPDATE ... WHERE id = 1 hits it.
INSERT INTO system_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
