package models

import (
	"encoding/json"
	"time"
)

// StateSnapshot represents a point-in-time capture of a workspace's Terraform state.
// The snapshot_data field stores a simplified representation of the state including
// resource types, counts, and metadata as JSONB.
type StateSnapshot struct {
	ID               string          `db:"id" json:"id"`
	OrganizationID   string          `db:"organization_id" json:"organization_id"`
	SourceID         *string         `db:"source_id" json:"source_id,omitempty"`
	WorkspaceName    string          `db:"workspace_name" json:"workspace_name"`
	WorkspaceID      *string         `db:"workspace_id" json:"workspace_id,omitempty"`
	SnapshotData     json.RawMessage `db:"snapshot_data" json:"snapshot_data"`
	ResourceCount    int             `db:"resource_count" json:"resource_count"`
	RUMCount         int             `db:"rum_count" json:"rum_count"`
	TerraformVersion *string         `db:"terraform_version" json:"terraform_version,omitempty"`
	StateSerial      *int            `db:"state_serial" json:"state_serial,omitempty"`
	CapturedAt       time.Time       `db:"captured_at" json:"captured_at"`
}

// SnapshotData is the structured representation of the data stored within
// the snapshot_data JSONB column. It captures resource type breakdowns and
// state metadata for later comparison.
type SnapshotData struct {
	ResourceTypes    map[string]int `json:"resource_types"`
	TerraformVersion string         `json:"terraform_version,omitempty"`
	ResourceCount    int            `json:"resource_count"`
	RUMCount         int            `json:"rum_count"`
	ManagedCount     int            `json:"managed_count"`
	DataSourceCount  int            `json:"data_source_count"`
	StateSerial      int            `json:"state_serial,omitempty"`
}
