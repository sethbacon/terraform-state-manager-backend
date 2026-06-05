package repositories

import identitystore "github.com/sethbacon/terraform-suite-identity/identity/store"

// APIKeyRepository is aliased from the shared identity store.
type APIKeyRepository = identitystore.APIKeyRepository

// NewAPIKeyRepository constructs an APIKeyRepository over the given connection.
var NewAPIKeyRepository = identitystore.NewAPIKeyRepository
