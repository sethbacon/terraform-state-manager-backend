package models

import identitymodels "github.com/sethbacon/terraform-suite-identity/identity/models"

// Organization, OrganizationMember and OrganizationMemberWithRole are aliased
// from the shared identity module.
type (
	Organization               = identitymodels.Organization
	OrganizationMember         = identitymodels.OrganizationMember
	OrganizationMemberWithRole = identitymodels.OrganizationMemberWithRole
)
