package models

import (
	"encoding/json"
	"time"
)

// SourceType constants for supported state backends
const (
	SourceTypeHCPTerraform = "hcp_terraform"
	SourceTypeAzureBlob    = "azure_blob"
	SourceTypeS3           = "s3"
	SourceTypeGCS          = "gcs"
	SourceTypeConsul       = "consul"
	SourceTypePG           = "pg"
	SourceTypeKubernetes   = "kubernetes"
	SourceTypeHTTP         = "http"
	SourceTypeLocal        = "local"
)

// SourceConfig is a type alias for json.RawMessage to hold backend-specific configuration.
type SourceConfig = json.RawMessage

// StateSource represents a configured Terraform state backend source.
type StateSource struct {
	ID             string          `db:"id" json:"id"`
	OrganizationID string          `db:"organization_id" json:"organization_id"`
	Name           string          `db:"name" json:"name"`
	SourceType     string          `db:"source_type" json:"source_type"`
	Config         json.RawMessage `db:"config" json:"config"`
	IsActive       bool            `db:"is_active" json:"is_active"`
	LastTestedAt   *time.Time      `db:"last_tested_at" json:"last_tested_at,omitempty"`
	LastTestStatus *string         `db:"last_test_status" json:"last_test_status,omitempty"`
	CreatedBy      *string         `db:"created_by" json:"created_by,omitempty"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at" json:"updated_at"`
}

// StateSourceCreateRequest is the API binding for creating a new state source.
type StateSourceCreateRequest struct {
	Name       string          `json:"name" binding:"required"`
	SourceType string          `json:"source_type" binding:"required"`
	Config     json.RawMessage `json:"config" binding:"required"`
	IsActive   *bool           `json:"is_active,omitempty"`
}

// StateSourceUpdateRequest is the API binding for updating an existing state source.
type StateSourceUpdateRequest struct {
	Name       *string          `json:"name,omitempty"`
	SourceType *string          `json:"source_type,omitempty"`
	Config     *json.RawMessage `json:"config,omitempty"`
	IsActive   *bool            `json:"is_active,omitempty"`
}
