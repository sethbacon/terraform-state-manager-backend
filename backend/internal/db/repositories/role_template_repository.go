package repositories

import identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"

// RoleTemplateRepository is aliased from the shared identity store.
type RoleTemplateRepository = identitystore.RoleTemplateRepository

// NewRoleTemplateRepository constructs a RoleTemplateRepository over the given connection.
var NewRoleTemplateRepository = identitystore.NewRoleTemplateRepository
