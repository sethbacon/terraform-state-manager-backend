-- Completeness markers on a drift record: what the check did NOT do.
--
-- The counts answer "how much drift", but nothing answered "did we actually
-- finish looking". @4cloudguru/terraform-drift-contract emits five markers
-- alongside every summary and both receivers dropped all five, so a stored
-- record could not distinguish "we checked and it was clean" from "we never
-- finished checking".
--
--   unparseable     the document was not a plan at all (no resource_changes),
--                   so zero counts are ignorance, not a clean result. This is
--                   the fail-open one: without it an unreadable plan looks
--                   exactly like a verified-clean plan.
--   truncated       a bound was reached, so the absence of a resource from the
--                   summary is not evidence of its absence.
--   omitted_entries summary rows dropped by the 500-entry bound, and
--   omitted_attrs   attributes dropped by the 50-per-entry bound — by how much.
--   unmasked        at least one change carried NEITHER sensitivity mirror, so
--                   nothing was redacted against it.
--
-- Defaults describe a complete, readable, fully-masked check, which is what
-- every pre-existing row was implicitly asserting and what a producer too old
-- to send the markers still means. Booleans and counters rather than a JSONB
-- envelope: these are filter predicates ("show me records nobody finished
-- checking"), matching added/changed/destroyed alongside them.
ALTER TABLE drift_records
    ADD COLUMN IF NOT EXISTS truncated       BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS omitted_entries INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS omitted_attrs   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS unparseable     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS unmasked        BOOLEAN NOT NULL DEFAULT false;

-- "Which records did we not finish checking?" is the question these exist to
-- answer, and it is asked across the whole table rather than per state.
CREATE INDEX IF NOT EXISTS idx_drift_records_incomplete
    ON drift_records (last_detected_at DESC) WHERE unparseable OR truncated;
