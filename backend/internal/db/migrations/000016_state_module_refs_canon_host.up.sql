-- Stage 2 of the canonical-host fix. A STORED generated column holding the
-- canonical form of registry_host so the suite "Consumed by" join:
--   * matches rows captured BEFORE host canonicalization (Stage 1) with no
--     backfill job and no ingest pause — Postgres derives the value at migrate
--     time and on every future write; and
--   * is case / default-port (:80,:443) / trailing-dot insensitive.
-- The Go capture and read paths already canonicalize; this column is the
-- engine-level safety net that also rescues legacy rows. IDN/punycode folding
-- is not expressible in pure SQL and is handled for new rows by the Go
-- canonicalizer, so non-ASCII legacy hosts are only case/port/dot-folded here.
-- The raw registry_host is preserved unchanged as the audit/provenance value.
ALTER TABLE state_module_refs
    ADD COLUMN registry_host_canon TEXT
    GENERATED ALWAYS AS (
        regexp_replace(lower(regexp_replace(registry_host, ':(80|443)$', '')), '[.]$', '')
    ) STORED;

-- Repoint the cross-app "consumed by" index at the canonical column.
DROP INDEX IF EXISTS idx_state_module_refs_host_module;
CREATE INDEX IF NOT EXISTS idx_state_module_refs_host_module
    ON state_module_refs (registry_host_canon, module_source);
