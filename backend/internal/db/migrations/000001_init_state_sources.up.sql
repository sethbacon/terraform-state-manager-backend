-- Initial schema: state_sources is the foundational table — one row per configured
-- connection to a backend where Terraform state already lives (HCP/TFC, Azure Blob,
-- S3, GCS, local, Git). Secrets are stored encrypted; non-secret settings and
-- org/workspace/prefix scoping are kept as JSONB.
CREATE TABLE IF NOT EXISTS state_sources (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  TEXT NOT NULL,
    type                  TEXT NOT NULL, -- hcp | azureblob | s3 | gcs | local | git
    endpoint              TEXT,
    config                JSONB NOT NULL DEFAULT '{}'::jsonb,
    encrypted_credentials BYTEA,
    scope                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_state_sources_name ON state_sources (name);
