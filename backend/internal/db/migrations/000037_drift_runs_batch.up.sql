-- Repo-level fan-out dispatch (drift-fleet-scale.md Phase 1): one ADO/GitHub
-- run can now plan several targets (apps) and report one callback PER target,
-- each keeping its own drift_runs row, its own one-shot callback token and its
-- own TTL. batch_id groups the rows a single dispatch produced; ci_run_id/
-- ci_run_url record the CI run the dispatch started, read back from the
-- dispatch API's own response (never from the callback body).
--
-- Nullable, no default: ADD COLUMN with neither is catalog-only (no table
-- rewrite, no ACCESS EXCLUSIVE hold proportional to the table's size).
ALTER TABLE drift_runs
    ADD COLUMN IF NOT EXISTS batch_id   UUID,
    ADD COLUMN IF NOT EXISTS ci_run_id  TEXT,
    ADD COLUMN IF NOT EXISTS ci_run_url TEXT;

-- Groups the rows one fan-out dispatch produced. Partial on IS NOT NULL: a
-- single-target dispatch leaves batch_id NULL (its DriftBatch.BatchID is the
-- run id itself, per the plan's back-compat rule), so indexing NULL would grow
-- the index with every legacy-shaped run for a predicate nothing queries.
CREATE INDEX IF NOT EXISTS idx_drift_runs_batch
    ON drift_runs (batch_id) WHERE batch_id IS NOT NULL;

-- Backs "what was the last run for this state" (coverage/Phase 4, and the
-- fan-out dispatcher's own duplicate-target guard), so DISTINCT ON (state_key)
-- ordered by created_at does not fall back to a sequential scan per source.
CREATE INDEX IF NOT EXISTS idx_drift_runs_state_created
    ON drift_runs (source_id, state_key, created_at DESC);
