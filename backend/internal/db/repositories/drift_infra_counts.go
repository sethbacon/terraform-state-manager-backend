package repositories

import "encoding/json"

// InfraDrift carries the drift contract's second triplet — resources changed
// OUTSIDE Terraform (resource_drift), as opposed to the Added/Changed/Destroyed
// fields already on DriftRun and Detection, which count a plan's
// resource_changes (edits nobody has applied yet). See migration 000039.
//
// Named and grouped the way Completeness is, for the same reason: one shape
// carried through the wire DTOs, the Detection bound for a record, and the
// UpdateResult call for a run, so a producer that starts sending these fields
// reaches storage by construction rather than by a hand-copied parameter list
// that can fall behind.
//
// The zero value is exactly what an older runner (or the ingest path before the
// driftingest Go mirror gains these fields) produces: no infra drift observed,
// no summary. That is indistinguishable from "the contract computed zero infra
// drift" by design — the same ambiguity Added/Changed/Destroyed already have,
// and not a new one this type introduces.
type InfraDrift struct {
	Added     int
	Changed   int
	Destroyed int
	Summary   json.RawMessage
}
