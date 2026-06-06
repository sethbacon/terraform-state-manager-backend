package models

import identitymodels "github.com/sethbacon/terraform-suite-identity/identity/models"

// Organization, OrganizationMember and the membership view types are aliased
// from the shared identity module.
type (
	Organization               = identitymodels.Organization
	OrganizationMember         = identitymodels.OrganizationMember
	OrganizationMemberWithUser = identitymodels.OrganizationMemberWithUser
	UserMembership             = identitymodels.UserMembership
)
