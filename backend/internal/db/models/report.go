package models

import "time"

// Report represents a generated report stored in the system.
type Report struct {
	ID             string    `db:"id" json:"id"`
	OrganizationID string    `db:"organization_id" json:"organization_id"`
	RunID          *string   `db:"run_id" json:"run_id,omitempty"`
	Name           string    `db:"name" json:"name"`
	Format         string    `db:"format" json:"format"`
	StoragePath    string    `db:"storage_path" json:"storage_path"`
	FileSizeBytes  *int64    `db:"file_size_bytes" json:"file_size_bytes,omitempty"`
	GeneratedBy    *string   `db:"generated_by" json:"generated_by,omitempty"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}

// ReportCreateRequest is the API binding for generating a new report.
type ReportCreateRequest struct {
	RunID  string `json:"run_id" binding:"required"`
	Format string `json:"format" binding:"required"`
	Name   string `json:"name"`
}
