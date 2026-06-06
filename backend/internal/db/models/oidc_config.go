// Package models - oidc_config.go aliases the persisted OIDCConfig identity type
// from the shared module and keeps the API DTOs (group mapping input, response)
// app-side, matching the registry's identity surface 1:1. OIDCConfigToResponse is
// a free function because methods cannot be attached to an aliased type.
package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	identitymodels "github.com/sethbacon/terraform-suite-identity/identity/models"
)

// OIDCConfig holds OIDC provider configuration stored in the database. The
// GetScopes / Get+SetGroupMappingConfig methods come along with the alias.
type OIDCConfig = identitymodels.OIDCConfig

// OIDCGroupMapping maps a single IdP group claim value to an organization and role
// template. It mirrors the identity type but is defined locally so swagger can
// document it (swag cannot resolve type aliases into the external identity
// module). Convert with ToIdentityGroupMappings / fromIdentityGroupMappings.
type OIDCGroupMapping struct {
	Group        string `json:"group"`
	Organization string `json:"organization"`
	Role         string `json:"role"`
}

// ToIdentityGroupMappings converts API group mappings to the identity model type
// accepted by OIDCConfig.SetGroupMappingConfig.
func ToIdentityGroupMappings(in []OIDCGroupMapping) []identitymodels.OIDCGroupMapping {
	out := make([]identitymodels.OIDCGroupMapping, len(in))
	for i, m := range in {
		out[i] = identitymodels.OIDCGroupMapping{Group: m.Group, Organization: m.Organization, Role: m.Role}
	}
	return out
}

// fromIdentityGroupMappings converts identity group mappings to the local type.
func fromIdentityGroupMappings(in []identitymodels.OIDCGroupMapping) []OIDCGroupMapping {
	out := make([]OIDCGroupMapping, len(in))
	for i, m := range in {
		out[i] = OIDCGroupMapping{Group: m.Group, Organization: m.Organization, Role: m.Role}
	}
	return out
}

// OIDCGroupMappingInput is used for updating only the group mapping configuration.
type OIDCGroupMappingInput struct {
	GroupClaimName string             `json:"group_claim_name"`
	GroupMappings  []OIDCGroupMapping `json:"group_mappings"`
	DefaultRole    string             `json:"default_role"`
}

// OIDCConfigResponse is the API response for OIDC configuration (no secrets).
type OIDCConfigResponse struct {
	ID             uuid.UUID              `json:"id"`
	Name           string                 `json:"name"`
	ProviderType   string                 `json:"provider_type"`
	IssuerURL      string                 `json:"issuer_url"`
	ClientID       string                 `json:"client_id"`
	RedirectURL    string                 `json:"redirect_url"`
	Scopes         []string               `json:"scopes"`
	IsActive       bool                   `json:"is_active"`
	GroupClaimName string                 `json:"group_claim_name,omitempty"`
	GroupMappings  []OIDCGroupMapping     `json:"group_mappings,omitempty"`
	DefaultRole    string                 `json:"default_role,omitempty"`
	ExtraConfig    map[string]interface{} `json:"extra_config,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
	UpdatedAt      time.Time              `json:"updated_at"`
	CreatedBy      *uuid.UUID             `json:"created_by,omitempty"`
	UpdatedBy      *uuid.UUID             `json:"updated_by,omitempty"`
}

// OIDCConfigToResponse converts an OIDCConfig to a safe API response (no secrets).
func OIDCConfigToResponse(c *OIDCConfig) *OIDCConfigResponse {
	resp := &OIDCConfigResponse{
		ID:           c.ID,
		Name:         c.Name,
		ProviderType: c.ProviderType,
		IssuerURL:    c.IssuerURL,
		ClientID:     c.ClientID,
		RedirectURL:  c.RedirectURL,
		IsActive:     c.IsActive,
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}

	// Parse scopes from JSONB.
	if len(c.Scopes) > 0 {
		_ = json.Unmarshal(c.Scopes, &resp.Scopes) // nolint:errcheck
	}
	if resp.Scopes == nil {
		resp.Scopes = []string{"openid", "email", "profile"}
	}

	// Parse extra config from JSONB — expose group mapping as first-class fields.
	if len(c.ExtraConfig) > 0 {
		_ = json.Unmarshal(c.ExtraConfig, &resp.ExtraConfig) // nolint:errcheck
		var mappings []identitymodels.OIDCGroupMapping
		resp.GroupClaimName, mappings, resp.DefaultRole = c.GetGroupMappingConfig()
		resp.GroupMappings = fromIdentityGroupMappings(mappings)
	}

	if c.CreatedBy.Valid {
		resp.CreatedBy = &c.CreatedBy.UUID
	}
	if c.UpdatedBy.Valid {
		resp.UpdatedBy = &c.UpdatedBy.UUID
	}

	return resp
}
