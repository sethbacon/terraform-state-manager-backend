package repositories

import identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"

// AuditRepository is aliased from the shared identity store.
type AuditRepository = identitystore.AuditRepository

// NewAuditRepository constructs an AuditRepository over the given connection.
var NewAuditRepository = identitystore.NewAuditRepository
