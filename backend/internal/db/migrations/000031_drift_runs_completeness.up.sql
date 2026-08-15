-- Completeness markers on a drift RUN: what that check did NOT do.
--
-- 000029 put the drift contract's five markers on drift_records and stopped
-- there, deliberately. The record answers "is this state drifted, and was the
-- finding complete?" — but per-RUN history asks a different question, and until
-- now a run row could not answer it: added/changed/destroyed all zero reads
-- identically for "we planned, and nothing had drifted" and for "we never
-- finished looking". That is the same distinction 000029 made load-bearing,
-- missing one layer up.
--
-- WHY THE RECORD CANNOT ANSWER IT FOR THE RUN. Deriving a run's completeness
-- from its records is not merely inconvenient, it is impossible for exactly the
-- runs that matter most:
--
--   * A clean run writes no record. It calls ResolveClean (or finds nothing
--     open) and stores no markers anywhere, so a clean-but-truncated check
--     leaves no trace at all.
--   * An unparseable run touches no record ON PURPOSE — that is the fail-open
--     000029/#382 closed — so the run row is the only place the fact that
--     nothing was verified can be written down.
--   * Records are overwritten in place on re-detection (markers included, like
--     the counts beside them), so even where a record exists it describes the
--     LATEST observation, never the run three checks ago. Per-run history needs
--     the account that run gave, not the account the next run overwrote it with.
--   * A run may carry no source_id at all (it is optional at dispatch), and the
--     (source_id, state_key) pair is the record identity — so such a run maps to
--     no record in either direction.
--
-- SHAPE. A drift run names exactly ONE state_key, so a run's completeness is not
-- an aggregate over many records; it is the single check's own five markers, and
-- the honest shape is the same five columns the record carries, with the same
-- names, types and defaults. Typed columns rather than a JSONB envelope for
-- 000029's reason, which holds here unchanged: they sit beside added, changed,
-- destroyed and drifted — already typed, already filter predicates — and a run
-- row that described completeness differently from a record row would make the
-- two impossible to compare without unpacking one of them first.
--
-- Defaults describe a complete, readable, fully-masked check: what every
-- pre-existing row was implicitly asserting, and what a producer too old to send
-- the markers still means.
--
-- No partial index here, unlike 000029. That index backs a question the record
-- plane is actually asked ("which findings did nobody finish checking?"); runs
-- are listed by status and windowed by created_at, and no query filters on these
-- yet. An index for a predicate nothing selects on is cost without a reader.
ALTER TABLE drift_runs
    ADD COLUMN IF NOT EXISTS truncated       BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS omitted_entries INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS omitted_attrs   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS unparseable     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS unmasked        BOOLEAN NOT NULL DEFAULT false;
