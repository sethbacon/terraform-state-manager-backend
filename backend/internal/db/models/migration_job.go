package models

import (
	"encoding/json"
	"time"
)

// MigrationStatus constants for migration job states.
const (
	MigrationStatusPending   = "pending"
	MigrationStatusRunning   = "running"
	MigrationStatusCompleted = "completed"
	MigrationStatusFailed    = "failed"
	MigrationStatusCancelled = "cancelled"
)

// MigrationJob represents a storage migration operation between backends.
type MigrationJob struct {
	ID             string          `db:"id" json:"id"`
	OrganizationID string          `db:"organization_id" json:"organization_id"`
	Name           string          `db:"name" json:"name"`
	SourceBackend  string          `db:"source_backend" json:"source_backend"`
	SourceConfig   json.RawMessage `db:"source_config" json:"source_config"`
	TargetBackend  string          `db:"target_backend" json:"target_backend"`
	TargetConfig   json.RawMessage `db:"target_config" json:"target_config"`
	Status         string          `db:"status" json:"status"`
	TotalFiles     int             `db:"total_files" json:"total_files"`
	MigratedFiles  int             `db:"migrated_files" json:"migrated_files"`
	FailedFiles    int             `db:"failed_files" json:"failed_files"`
	SkippedFiles   int             `db:"skipped_files" json:"skipped_files"`
	ErrorLog       json.RawMessage `db:"error_log" json:"error_log"`
	DryRun         bool            `db:"dry_run" json:"dry_run"`
	StartedAt      *time.Time      `db:"started_at" json:"started_at,omitempty"`
	CompletedAt    *time.Time      `db:"completed_at" json:"completed_at,omitempty"`
	CreatedBy      *string         `db:"created_by" json:"created_by,omitempty"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at" json:"updated_at"`
}

// MigrationJobCreateRequest is the API binding for creating a new migration job.
type MigrationJobCreateRequest struct {
	Name          string          `json:"name" binding:"required"`
	SourceBackend string          `json:"source_backend" binding:"required"`
	SourceConfig  json.RawMessage `json:"source_config" binding:"required"`
	TargetBackend string          `json:"target_backend" binding:"required"`
	TargetConfig  json.RawMessage `json:"target_config" binding:"required"`
	DryRun        *bool           `json:"dry_run,omitempty"`
}
