-- Phase 2 edit plane: pre-edit backups and an audit trail of edits.

-- state_backups holds an immutable copy of a state file captured immediately
-- before an edit, so any change can be reverted one click.
CREATE TABLE IF NOT EXISTS state_backups (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id  UUID NOT NULL REFERENCES state_sources(id) ON DELETE CASCADE,
    state_key  TEXT NOT NULL,
    data       BYTEA NOT NULL,
    serial     BIGINT,
    created_by TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_state_backups_source_key
    ON state_backups (source_id, state_key, created_at DESC);

-- state_edits records every mutation for audit: who, what, the backup taken,
-- serial transition, and the outcome.
CREATE TABLE IF NOT EXISTS state_edits (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id     UUID NOT NULL REFERENCES state_sources(id) ON DELETE CASCADE,
    state_key     TEXT NOT NULL,
    operation     TEXT NOT NULL, -- raw_replace | restore
    actor         TEXT,
    backup_id     UUID REFERENCES state_backups(id) ON DELETE SET NULL,
    before_serial BIGINT,
    after_serial  BIGINT,
    result        TEXT NOT NULL, -- success | failed
    detail        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_state_edits_source_key
    ON state_edits (source_id, state_key, created_at DESC);
