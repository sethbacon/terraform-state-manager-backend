package models

import (
	"encoding/json"
	"time"
)

// PolicyType constants for supported compliance policy types.
const (
	PolicyTypeTagging = "tagging"
	PolicyTypeNaming  = "naming"
	PolicyTypeVersion = "version"
	PolicyTypeCustom  = "custom"
)

// CompliancePolicy represents a compliance policy that defines rules for resource validation.
type CompliancePolicy struct {
	ID             string          `db:"id" json:"id"`
	OrganizationID string          `db:"organization_id" json:"organization_id"`
	Name           string          `db:"name" json:"name"`
	PolicyType     string          `db:"policy_type" json:"policy_type"`
	Config         json.RawMessage `db:"config" json:"config"`
	Severity       string          `db:"severity" json:"severity"`
	IsActive       bool            `db:"is_active" json:"is_active"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at" json:"updated_at"`
}

// CompliancePolicyCreateRequest is the API binding for creating a new compliance policy.
type CompliancePolicyCreateRequest struct {
	Name       string          `json:"name" binding:"required"`
	PolicyType string          `json:"policy_type" binding:"required"`
	Config     json.RawMessage `json:"config" binding:"required"`
	Severity   string          `json:"severity,omitempty"`
	IsActive   *bool           `json:"is_active,omitempty"`
}

// CompliancePolicyUpdateRequest is the API binding for updating an existing compliance policy.
type CompliancePolicyUpdateRequest struct {
	Name       *string          `json:"name,omitempty"`
	PolicyType *string          `json:"policy_type,omitempty"`
	Config     *json.RawMessage `json:"config,omitempty"`
	Severity   *string          `json:"severity,omitempty"`
	IsActive   *bool            `json:"is_active,omitempty"`
}
