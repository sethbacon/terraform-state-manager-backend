-- Contract: separate infra drift (resource_drift) from unapplied changes
-- (resource_changes) (drift-fleet-scale.md Phase 5 item 4).
--
-- drift_runs and drift_records already carry added/changed/destroyed +
-- summary, computed from a plan's resource_changes -- edits nobody has
-- applied yet. terraform-drift-contract 1.3.0 adds a second, independent
-- triplet computed from resource_drift -- changes made OUTSIDE Terraform
-- (hand edits, console changes) that a plan surfaces without anyone having
-- proposed them. The two describe different problems on the same state, so
-- they get their own columns rather than being folded into the existing
-- ones: folding would make a state that is only hand-drifted
-- indistinguishable from one with a pending apply.
--
-- Nullable, DEFAULT 0/NULL: ADD COLUMN with a constant default is
-- catalog-only on PostgreSQL 11+ (no table rewrite, no ACCESS EXCLUSIVE hold
-- proportional to the table's size), matching 000037's note for the same
-- reason. The Go mirror (internal/services/driftingest) and the payload
-- DTOs (driftRunResultPayload, driftIngestPayload) decode the new fields
-- leniently, so an older runner that sends none of them writes exactly 0 /
-- NULL here -- metadata-only, not a breaking change.
ALTER TABLE drift_runs
    ADD COLUMN IF NOT EXISTS drift_added     INTEGER DEFAULT 0,
    ADD COLUMN IF NOT EXISTS drift_changed   INTEGER DEFAULT 0,
    ADD COLUMN IF NOT EXISTS drift_destroyed INTEGER DEFAULT 0,
    ADD COLUMN IF NOT EXISTS drift_summary   JSONB;

ALTER TABLE drift_records
    ADD COLUMN IF NOT EXISTS drift_added     INTEGER DEFAULT 0,
    ADD COLUMN IF NOT EXISTS drift_changed   INTEGER DEFAULT 0,
    ADD COLUMN IF NOT EXISTS drift_destroyed INTEGER DEFAULT 0,
    ADD COLUMN IF NOT EXISTS drift_summary   JSONB;
