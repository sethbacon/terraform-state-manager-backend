package models

import (
	"encoding/json"
	"time"
)

// RunStatus constants for analysis run states
const (
	RunStatusPending   = "pending"
	RunStatusRunning   = "running"
	RunStatusCompleted = "completed"
	RunStatusFailed    = "failed"
	RunStatusCancelled = "cancelled"
)

// TriggerType constants for how an analysis run was initiated
const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
	TriggerAPI       = "api"
)

// AnalysisRun represents a single execution of a state analysis across workspaces.
type AnalysisRun struct {
	ID               string          `db:"id" json:"id"`
	OrganizationID   string          `db:"organization_id" json:"organization_id"`
	SourceID         *string         `db:"source_id" json:"source_id,omitempty"`
	Status           string          `db:"status" json:"status"`
	TriggerType      string          `db:"trigger_type" json:"trigger_type"`
	Config           json.RawMessage `db:"config" json:"config"`
	StartedAt        *time.Time      `db:"started_at" json:"started_at,omitempty"`
	CompletedAt      *time.Time      `db:"completed_at" json:"completed_at,omitempty"`
	TotalWorkspaces  int             `db:"total_workspaces" json:"total_workspaces"`
	SuccessfulCount  int             `db:"successful_count" json:"successful_count"`
	FailedCount      int             `db:"failed_count" json:"failed_count"`
	TotalRUM         int             `db:"total_rum" json:"total_rum"`
	TotalManaged     int             `db:"total_managed" json:"total_managed"`
	TotalResources   int             `db:"total_resources" json:"total_resources"`
	TotalDataSources int             `db:"total_data_sources" json:"total_data_sources"`
	ErrorMessage     *string         `db:"error_message" json:"error_message,omitempty"`
	PerformanceMS    *int            `db:"performance_ms" json:"performance_ms,omitempty"`
	TriggeredBy      *string         `db:"triggered_by" json:"triggered_by,omitempty"`
	CreatedAt        time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time       `db:"updated_at" json:"updated_at"`
}

// AnalysisRunCreateRequest is the API binding for creating a new analysis run.
type AnalysisRunCreateRequest struct {
	SourceID    *string          `json:"source_id,omitempty"`
	TriggerType string           `json:"trigger_type" binding:"required"`
	Config      *json.RawMessage `json:"config,omitempty"`
}
