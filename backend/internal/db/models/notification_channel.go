package models

import (
	"encoding/json"
	"time"
)

// ChannelType constants for supported notification channel types.
const (
	ChannelTypeWebhook = "webhook"
	ChannelTypeSlack   = "slack"
	ChannelTypeTeams   = "teams"
	ChannelTypeEmail   = "email"
)

// NotificationChannel represents a configured notification delivery channel.
type NotificationChannel struct {
	ID             string          `db:"id" json:"id"`
	OrganizationID string          `db:"organization_id" json:"organization_id"`
	Name           string          `db:"name" json:"name"`
	ChannelType    string          `db:"channel_type" json:"channel_type"`
	Config         json.RawMessage `db:"config" json:"config"`
	IsActive       bool            `db:"is_active" json:"is_active"`
	CreatedAt      time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at" json:"updated_at"`
}

// NotificationChannelCreateRequest is the API binding for creating a new notification channel.
type NotificationChannelCreateRequest struct {
	Name        string          `json:"name" binding:"required"`
	ChannelType string          `json:"channel_type" binding:"required"`
	Config      json.RawMessage `json:"config" binding:"required"`
	IsActive    *bool           `json:"is_active,omitempty"`
}

// NotificationChannelUpdateRequest is the API binding for updating an existing notification channel.
type NotificationChannelUpdateRequest struct {
	Name        *string          `json:"name,omitempty"`
	ChannelType *string          `json:"channel_type,omitempty"`
	Config      *json.RawMessage `json:"config,omitempty"`
	IsActive    *bool            `json:"is_active,omitempty"`
}
