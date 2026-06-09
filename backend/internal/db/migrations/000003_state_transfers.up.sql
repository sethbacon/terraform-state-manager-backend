-- Phase 2 transfer plane: cross-source backup (copy) and migrate (move + verify).
CREATE TABLE IF NOT EXISTS state_transfers (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    mode             TEXT NOT NULL, -- backup | migrate
    source_id        UUID NOT NULL REFERENCES state_sources(id) ON DELETE CASCADE,
    source_key       TEXT NOT NULL,
    target_source_id UUID NOT NULL REFERENCES state_sources(id) ON DELETE CASCADE,
    target_key       TEXT NOT NULL,
    status           TEXT NOT NULL, -- success | verification_failed | failed
    verified         BOOLEAN,
    decommissioned   BOOLEAN NOT NULL DEFAULT false,
    detail           TEXT,
    actor            TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_state_transfers_source ON state_transfers (source_id, created_at DESC);
