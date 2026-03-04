package models

import "time"

// RetentionPolicy defines how long state backups are retained for an organization.
type RetentionPolicy struct {
	ID             string    `db:"id" json:"id"`
	OrganizationID string    `db:"organization_id" json:"organization_id"`
	Name           string    `db:"name" json:"name"`
	MaxAgeDays     *int      `db:"max_age_days" json:"max_age_days,omitempty"`
	MaxCount       *int      `db:"max_count" json:"max_count,omitempty"`
	IsDefault      bool      `db:"is_default" json:"is_default"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

// RetentionPolicyCreateRequest is the API binding for creating a new retention policy.
type RetentionPolicyCreateRequest struct {
	Name       string `json:"name" binding:"required"`
	MaxAgeDays *int   `json:"max_age_days,omitempty"`
	MaxCount   *int   `json:"max_count,omitempty"`
	IsDefault  *bool  `json:"is_default,omitempty"`
}

// RetentionPolicyUpdateRequest is the API binding for updating an existing retention policy.
type RetentionPolicyUpdateRequest struct {
	Name       *string `json:"name,omitempty"`
	MaxAgeDays *int    `json:"max_age_days,omitempty"`
	MaxCount   *int    `json:"max_count,omitempty"`
	IsDefault  *bool   `json:"is_default,omitempty"`
}
