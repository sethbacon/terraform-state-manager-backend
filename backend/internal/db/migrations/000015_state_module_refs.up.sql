-- Module provenance captured from ingested Terraform plans: which registry
-- modules (source address + version constraint) a given state calls. Populated
-- best-effort on POST /drift/ingest when a full `terraform show -json` plan is
-- pushed; absent for sources that only post pre-computed drift counts, so the
-- UI must treat missing provenance as normal. The (source_id, state_key) pair
-- matches TSM's drift-plane record identity. module_source/registry_host are
-- plain strings (the cross-app join key) — no FK into any registry. A locked
-- module_version is NULL until a lockfile-upload contract exists; today only the
-- plan's version_constraint is available.
CREATE TABLE IF NOT EXISTS state_module_refs (
    source_id      UUID NOT NULL REFERENCES state_sources(id) ON DELETE CASCADE,
    state_key      TEXT NOT NULL,
    module_source  TEXT NOT NULL,   -- e.g. "terraform-aws-modules/vpc/aws"
    module_version TEXT,            -- locked version when known; NULL when only a constraint exists
    registry_host  TEXT NOT NULL,   -- e.g. "registry.terraform.io"; the host the join keys on
    observed_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Provenance is replaced wholesale per (source, state) ingest and listed by source.
CREATE INDEX IF NOT EXISTS idx_state_module_refs_source ON state_module_refs (source_id, state_key);
-- The cross-app "consumed by" query keys on (registry_host, module_source).
CREATE INDEX IF NOT EXISTS idx_state_module_refs_host_module ON state_module_refs (registry_host, module_source);
