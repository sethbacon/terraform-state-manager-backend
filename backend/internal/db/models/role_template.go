package models

import identitymodels "github.com/sethbacon/terraform-suite-identity/identity/models"

// RoleTemplate is aliased from the shared identity module.
type RoleTemplate = identitymodels.RoleTemplate

// PredefinedRoleTemplates returns TSM's built-in system role templates and their
// scope sets. This is the app-side half of the "identity-core + app-extended"
// model: the shared identity module is scope-agnostic and seeds these roles with
// identity-core scopes only, so each consuming app layers its own domain scopes.
//
// The scope sets here mirror the public-schema seed (migration 000001) exactly,
// so a role grants the same permissions whether identity data lives in the public
// schema (default) or the shared identity schema (cutover). See
// SeedSystemRoleTemplates, which applies these under the identity-schema cutover.
func PredefinedRoleTemplates() []RoleTemplate {
	adminDesc := "Full administrative access"
	analystDesc := "Analysis and reporting access"
	viewerDesc := "Read-only access"
	operatorDesc := "Operational access without administration"

	return []RoleTemplate{
		{
			Name:        "admin",
			DisplayName: "Administrator",
			Description: &adminDesc,
			Scopes: []string{
				"admin",
				"analysis:read", "analysis:write",
				"reports:read", "reports:write",
				"dashboard:read", "dashboard:write",
				"sources:read", "sources:write",
				"compliance:read", "compliance:write",
				"users:read", "users:write",
				"organizations:read", "organizations:write",
				"api_keys:read", "api_keys:write",
				"audit:read",
				"settings:read", "settings:write",
			},
			IsSystem: true,
		},
		{
			Name:        "analyst",
			DisplayName: "Analyst",
			Description: &analystDesc,
			Scopes: []string{
				"analysis:read", "analysis:write",
				"reports:read", "reports:write",
				"dashboard:read",
				"sources:read",
				"compliance:read",
			},
			IsSystem: true,
		},
		{
			Name:        "viewer",
			DisplayName: "Viewer",
			Description: &viewerDesc,
			Scopes: []string{
				"analysis:read",
				"reports:read",
				"dashboard:read",
				"sources:read",
			},
			IsSystem: true,
		},
		{
			Name:        "operator",
			DisplayName: "Operator",
			Description: &operatorDesc,
			Scopes: []string{
				"analysis:read", "analysis:write",
				"reports:read", "reports:write",
				"dashboard:read", "dashboard:write",
				"sources:read", "sources:write",
				"compliance:read", "compliance:write",
				"users:read",
				"organizations:read",
				"api_keys:read", "api_keys:write",
				"audit:read",
				"settings:read",
			},
			IsSystem: true,
		},
	}
}
