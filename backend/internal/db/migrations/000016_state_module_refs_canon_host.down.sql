-- Reverse 000016: drop the canonical column and restore the index to the raw
-- registry_host. Non-destructive — registry_host (the source of truth) is
-- untouched, so this is cleanly reversible.
DROP INDEX IF EXISTS idx_state_module_refs_host_module;
ALTER TABLE state_module_refs DROP COLUMN IF EXISTS registry_host_canon;
CREATE INDEX IF NOT EXISTS idx_state_module_refs_host_module
    ON state_module_refs (registry_host, module_source);
