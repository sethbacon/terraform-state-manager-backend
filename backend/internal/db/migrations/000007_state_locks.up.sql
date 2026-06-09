-- App-level advisory locks so every state source — including connectors without a
-- native lock (S3/GCS/Azure/HCP/git) — is mutually excluded during edits. The
-- UNIQUE(source_id, state_key) constraint makes acquisition atomic: a second
-- editor's INSERT fails while the first holds the lock.
CREATE TABLE IF NOT EXISTS state_locks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id   UUID NOT NULL REFERENCES state_sources(id) ON DELETE CASCADE,
    state_key   TEXT NOT NULL,
    actor       TEXT,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_id, state_key)
);
