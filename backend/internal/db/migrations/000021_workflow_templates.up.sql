-- Operator-managed CI workflow templates. Previously the drift/version-lab
-- workflow YAML was a hardcoded Go const served read-only; this table lets an
-- operator edit/add/replace a template per (provider, kind, profile) so it fits
-- their repository structure (e.g. Brunswick's per-app monorepo layout).
--
-- The handler resolves (provider, kind, profile) against this table first and
-- falls back to the embedded built-in when no row exists, so existing callers
-- keep working. Built-ins are seeded with profile='default' and is_builtin=true.
CREATE TABLE IF NOT EXISTS workflow_templates (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider    TEXT NOT NULL,
    kind        TEXT NOT NULL,
    profile     TEXT NOT NULL DEFAULT 'default',
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    content     TEXT NOT NULL,
    is_builtin  BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, kind, profile),
    CHECK (provider IN ('github_actions', 'azure_devops')),
    CHECK (kind IN ('drift', 'versionlab')),
    CHECK (profile ~ '^[A-Za-z0-9._-]+$')
);
