package repositories

import identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"

// AuditRepository and AuditFilters are aliased from the shared identity store.
type (
	AuditRepository = identitystore.AuditRepository
	AuditFilters    = identitystore.AuditFilters
)

// NewAuditRepository constructs an AuditRepository over the given connection.
var NewAuditRepository = identitystore.NewAuditRepository
