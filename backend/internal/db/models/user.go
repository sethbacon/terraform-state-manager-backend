package models

import identitymodels "github.com/sethbacon/terraform-suite-identity/identity/models"

// User and UserWithOrgRoles are aliased from the shared identity module so the
// types are owned in one place across the suite. Methods (GetAllowedScopes,
// HasAdminScope) come along with the alias.
type (
	User             = identitymodels.User
	UserWithOrgRoles = identitymodels.UserWithOrgRoles
)
