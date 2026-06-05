package repositories

import identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"

// OrganizationRepository is aliased from the shared identity store.
type OrganizationRepository = identitystore.OrganizationRepository

// NewOrganizationRepository constructs an OrganizationRepository over the given connection.
var NewOrganizationRepository = identitystore.NewOrganizationRepository
