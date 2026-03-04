package models

import (
	"encoding/json"
	"time"
)

// ComplianceStatus constants for compliance check outcomes.
const (
	ComplianceStatusPass    = "pass"
	ComplianceStatusFail    = "fail"
	ComplianceStatusWarning = "warning"
)

// ComplianceResult represents the result of a compliance policy check against a workspace.
type ComplianceResult struct {
	ID            string          `db:"id" json:"id"`
	PolicyID      string          `db:"policy_id" json:"policy_id"`
	RunID         string          `db:"run_id" json:"run_id"`
	WorkspaceName string          `db:"workspace_name" json:"workspace_name"`
	Status        string          `db:"status" json:"status"`
	Violations    json.RawMessage `db:"violations" json:"violations"`
	CreatedAt     time.Time       `db:"created_at" json:"created_at"`
}
