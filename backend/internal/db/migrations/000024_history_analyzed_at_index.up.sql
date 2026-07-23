-- PruneHistory deletes state_analysis_history rows by analyzed_at age every
-- sync cycle; without an index that is a sequential scan per cycle. History
-- lookups (latest row per source/state) also order by analyzed_at.
CREATE INDEX IF NOT EXISTS idx_state_analysis_history_analyzed_at
    ON state_analysis_history (analyzed_at);
