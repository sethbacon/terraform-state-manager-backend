package models

import "time"

// StateBackup represents a point-in-time backup of a Terraform state file.
type StateBackup struct {
	ID                string     `db:"id" json:"id"`
	OrganizationID    string     `db:"organization_id" json:"organization_id"`
	SourceID          *string    `db:"source_id" json:"source_id,omitempty"`
	WorkspaceName     string     `db:"workspace_name" json:"workspace_name"`
	WorkspaceID       *string    `db:"workspace_id" json:"workspace_id,omitempty"`
	StorageBackend    string     `db:"storage_backend" json:"storage_backend"`
	StoragePath       string     `db:"storage_path" json:"storage_path"`
	FileSizeBytes     *int64     `db:"file_size_bytes" json:"file_size_bytes,omitempty"`
	TerraformVersion  *string    `db:"terraform_version" json:"terraform_version,omitempty"`
	StateSerial       *int       `db:"state_serial" json:"state_serial,omitempty"`
	ChecksumSHA256    *string    `db:"checksum_sha256" json:"checksum_sha256,omitempty"`
	RetentionPolicyID *string    `db:"retention_policy_id" json:"retention_policy_id,omitempty"`
	ExpiresAt         *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	CreatedAt         time.Time  `db:"created_at" json:"created_at"`
}

// StateBackupCreateRequest is the API binding for creating a new state backup.
type StateBackupCreateRequest struct {
	SourceID      string `json:"source_id" binding:"required"`
	WorkspaceName string `json:"workspace_name" binding:"required"`
	WorkspaceID   string `json:"workspace_id,omitempty"`
}
