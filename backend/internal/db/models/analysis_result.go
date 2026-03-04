package models

import (
	"encoding/json"
	"time"
)

// ResultStatus constants for individual workspace analysis outcomes
const (
	ResultStatusSuccess = "success"
	ResultStatusFailed  = "failed"
	ResultStatusSkipped = "skipped"
)

// ErrorType constants for categorizing analysis failures
const (
	ErrorTypeStateNotFound    = "state_not_found"
	ErrorTypePermissionDenied = "permission_denied"
	ErrorTypeUnauthorized     = "unauthorized"
	ErrorTypeTimeout          = "timeout"
	ErrorTypeException        = "exception"
	ErrorTypeUnknown          = "unknown"
)

// AnalysisResult represents the result of analyzing a single workspace's Terraform state.
type AnalysisResult struct {
	ID                string          `db:"id" json:"id"`
	RunID             string          `db:"run_id" json:"run_id"`
	WorkspaceID       *string         `db:"workspace_id" json:"workspace_id,omitempty"`
	WorkspaceName     string          `db:"workspace_name" json:"workspace_name"`
	Organization      *string         `db:"organization" json:"organization,omitempty"`
	Status            string          `db:"status" json:"status"`
	ErrorType         *string         `db:"error_type" json:"error_type,omitempty"`
	ErrorMessage      *string         `db:"error_message" json:"error_message,omitempty"`
	TotalResources    int             `db:"total_resources" json:"total_resources"`
	ManagedCount      int             `db:"managed_count" json:"managed_count"`
	RUMCount          int             `db:"rum_count" json:"rum_count"`
	DataSourceCount   int             `db:"data_source_count" json:"data_source_count"`
	NullResourceCount int             `db:"null_resource_count" json:"null_resource_count"`
	ResourcesByType   json.RawMessage `db:"resources_by_type" json:"resources_by_type"`
	ResourcesByModule json.RawMessage `db:"resources_by_module" json:"resources_by_module"`
	ProviderAnalysis  json.RawMessage `db:"provider_analysis" json:"provider_analysis"`
	TerraformVersion  *string         `db:"terraform_version" json:"terraform_version,omitempty"`
	StateSerial       *int            `db:"state_serial" json:"state_serial,omitempty"`
	StateLineage      *string         `db:"state_lineage" json:"state_lineage,omitempty"`
	LastModified      *time.Time      `db:"last_modified" json:"last_modified,omitempty"`
	AnalysisMethod    *string         `db:"analysis_method" json:"analysis_method,omitempty"`
	RawStateHash      *string         `db:"raw_state_hash" json:"raw_state_hash,omitempty"`
	CreatedAt         time.Time       `db:"created_at" json:"created_at"`
}
