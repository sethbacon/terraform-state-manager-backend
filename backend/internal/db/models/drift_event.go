package models

import (
	"encoding/json"
	"time"
)

// DriftSeverity constants for categorizing the significance of a drift event.
const (
	DriftSeverityInfo     = "info"
	DriftSeverityWarning  = "warning"
	DriftSeverityCritical = "critical"
)

// DriftSource constants identify the origin of a drift event.
const (
	// DriftSourceSnapshot marks events detected by comparing two state snapshots.
	DriftSourceSnapshot = "snapshot"
	// DriftSourceCode marks events ingested from a Terraform plan JSON via the
	// external drift-ingest endpoint.
	DriftSourceCode = "code"
	// DriftSourceEnvironment marks events detected by comparing a Terraform
	// state's resources against their live cloud (e.g. Azure ARM) counterparts.
	DriftSourceEnvironment = "environment"
)

// DriftEvent records a detected change for a workspace. It covers both
// snapshot-vs-snapshot drift (DriftSourceSnapshot) and code drift ingested from
// a Terraform plan JSON (DriftSourceCode). The changes field stores a
// DriftChanges struct as JSONB.
type DriftEvent struct {
	ID             string          `db:"id" json:"id"`
	OrganizationID string          `db:"organization_id" json:"organization_id"`
	WorkspaceName  string          `db:"workspace_name" json:"workspace_name"`
	SnapshotBefore *string         `db:"snapshot_before" json:"snapshot_before,omitempty"`
	SnapshotAfter  *string         `db:"snapshot_after" json:"snapshot_after,omitempty"`
	Changes        json.RawMessage `db:"changes" json:"changes"`
	Severity       string          `db:"severity" json:"severity"`
	DriftSource    string          `db:"drift_source" json:"drift_source"`
	ExternalRef    *string         `db:"external_ref" json:"external_ref,omitempty"`
	DetectedAt     time.Time       `db:"detected_at" json:"detected_at"`
}

// ClassifyDriftSeverity determines the severity level of drift based on resource counts.
// - critical: resource removals detected
// - warning: resource additions or modifications detected
// - info: no meaningful change
func ClassifyDriftSeverity(added, removed, modified int) string {
	if removed > 0 {
		return DriftSeverityCritical
	}
	if added > 0 || modified > 0 {
		return DriftSeverityWarning
	}
	return DriftSeverityInfo
}
