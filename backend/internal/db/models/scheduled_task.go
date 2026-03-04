package models

import (
	"encoding/json"
	"time"
)

// TaskType constants for supported scheduled task types.
const (
	TaskTypeAnalysis = "analysis"
	TaskTypeReport   = "report"
	TaskTypeBackup   = "backup"
	TaskTypeSnapshot = "snapshot"
)

// TaskRunStatus constants for the outcome of a scheduled task execution.
const (
	TaskRunStatusSuccess = "success"
	TaskRunStatusFailed  = "failed"
	TaskRunStatusSkipped = "skipped"
)

// ScheduledTask represents a recurring task configuration stored in the database.
type ScheduledTask struct {
	ID             string          `db:"id" json:"id"`
	OrganizationID string          `db:"organization_id" json:"organization_id"`
	Name           string          `db:"name" json:"name"`
	TaskType       string          `db:"task_type" json:"task_type"`
	Schedule       string          `db:"schedule" json:"schedule"`
	Config         json.RawMessage `db:"config" json:"config"`
	IsActive       bool            `db:"is_active" json:"is_active"`
	LastRunAt      *time.Time      `db:"last_run_at" json:"last_run_at,omitempty"`
	NextRunAt      *time.Time      `db:"next_run_at" json:"next_run_at,omitempty"`
	LastRunStatus  *string         `db:"last_run_status" json:"last_run_status,omitempty"`
	CreatedBy      *string         `db:"created_by" json:"created_by,omitempty"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at" json:"updated_at"`
}

// ScheduledTaskCreateRequest is the API binding for creating a new scheduled task.
type ScheduledTaskCreateRequest struct {
	Name     string           `json:"name" binding:"required"`
	TaskType string           `json:"task_type" binding:"required"`
	Schedule string           `json:"schedule" binding:"required"`
	Config   *json.RawMessage `json:"config,omitempty"`
	IsActive *bool            `json:"is_active,omitempty"`
}

// ScheduledTaskUpdateRequest is the API binding for updating an existing scheduled task.
type ScheduledTaskUpdateRequest struct {
	Name     *string          `json:"name,omitempty"`
	TaskType *string          `json:"task_type,omitempty"`
	Schedule *string          `json:"schedule,omitempty"`
	Config   *json.RawMessage `json:"config,omitempty"`
	IsActive *bool            `json:"is_active,omitempty"`
}

// ValidTaskTypes returns the set of valid task type strings.
func ValidTaskTypes() map[string]bool {
	return map[string]bool{
		TaskTypeAnalysis: true,
		TaskTypeReport:   true,
		TaskTypeBackup:   true,
		TaskTypeSnapshot: true,
	}
}
