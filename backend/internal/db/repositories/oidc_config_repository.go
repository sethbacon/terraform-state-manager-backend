package repositories

import identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"

// OIDCConfigRepository is aliased from the shared identity store.
type OIDCConfigRepository = identitystore.OIDCConfigRepository

// NewOIDCConfigRepository constructs an OIDCConfigRepository over the given connection.
var NewOIDCConfigRepository = identitystore.NewOIDCConfigRepository
