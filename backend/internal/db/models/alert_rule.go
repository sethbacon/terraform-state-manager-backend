package models

import (
	"encoding/json"
	"time"
)

// RuleType constants for supported alert rule types.
const (
	RuleTypeStaleWorkspace  = "stale_workspace"
	RuleTypeResourceGrowth  = "resource_growth"
	RuleTypeRUMThreshold    = "rum_threshold"
	RuleTypeAnalysisFailure = "analysis_failure"
	RuleTypeDriftDetected   = "drift_detected"
	RuleTypeBackupFailure   = "backup_failure"
	RuleTypeVersionOutdated = "version_outdated"
)

// AlertRule represents a configured alert rule that defines conditions for triggering alerts.
type AlertRule struct {
	ID             string          `db:"id" json:"id"`
	OrganizationID string          `db:"organization_id" json:"organization_id"`
	Name           string          `db:"name" json:"name"`
	RuleType       string          `db:"rule_type" json:"rule_type"`
	Config         json.RawMessage `db:"config" json:"config"`
	Severity       string          `db:"severity" json:"severity"`
	ChannelIDs     json.RawMessage `db:"channel_ids" json:"channel_ids"`
	IsActive       bool            `db:"is_active" json:"is_active"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at" json:"updated_at"`
}

// AlertRuleCreateRequest is the API binding for creating a new alert rule.
type AlertRuleCreateRequest struct {
	Name       string          `json:"name" binding:"required"`
	RuleType   string          `json:"rule_type" binding:"required"`
	Config     json.RawMessage `json:"config" binding:"required"`
	Severity   string          `json:"severity,omitempty"`
	ChannelIDs json.RawMessage `json:"channel_ids,omitempty"`
	IsActive   *bool           `json:"is_active,omitempty"`
}

// AlertRuleUpdateRequest is the API binding for updating an existing alert rule.
type AlertRuleUpdateRequest struct {
	Name       *string          `json:"name,omitempty"`
	RuleType   *string          `json:"rule_type,omitempty"`
	Config     *json.RawMessage `json:"config,omitempty"`
	Severity   *string          `json:"severity,omitempty"`
	ChannelIDs *json.RawMessage `json:"channel_ids,omitempty"`
	IsActive   *bool            `json:"is_active,omitempty"`
}
