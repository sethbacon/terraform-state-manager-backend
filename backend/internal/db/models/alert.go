package models

import (
	"encoding/json"
	"time"
)

// AlertSeverity constants for alert severity levels.
const (
	AlertSeverityCritical = "critical"
	AlertSeverityWarning  = "warning"
	AlertSeverityInfo     = "info"
)

// Alert represents a triggered alert instance.
type Alert struct {
	ID             string          `db:"id" json:"id"`
	OrganizationID string          `db:"organization_id" json:"organization_id"`
	RuleID         *string         `db:"rule_id" json:"rule_id,omitempty"`
	WorkspaceName  string          `db:"workspace_name" json:"workspace_name"`
	Severity       string          `db:"severity" json:"severity"`
	Title          string          `db:"title" json:"title"`
	Description    string          `db:"description" json:"description"`
	Metadata       json.RawMessage `db:"metadata" json:"metadata"`
	IsAcknowledged bool            `db:"is_acknowledged" json:"is_acknowledged"`
	AcknowledgedBy *string         `db:"acknowledged_by" json:"acknowledged_by,omitempty"`
	AcknowledgedAt *time.Time      `db:"acknowledged_at" json:"acknowledged_at,omitempty"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
}
