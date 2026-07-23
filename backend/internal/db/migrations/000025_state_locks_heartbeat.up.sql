-- Stale-lock reaping keys on a heartbeat, not on acquisition age: a live
-- long-running edit renews renewed_at while it works, so it is never mistaken
-- for a crashed holder, while a real crash stops renewing and ages out.
ALTER TABLE state_locks
    ADD COLUMN IF NOT EXISTS renewed_at TIMESTAMPTZ NOT NULL DEFAULT now();
