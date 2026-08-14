package platformadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	idplatformadmin "github.com/sethbacon/terraform-suite-identity/identity/platformadmin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// identityResolver answers the carrier's "does this grant still name somebody?"
// question out of TSM's identity user store.
//
// It exists because the carrier holds NO foreign key to identity.users — it
// cannot, since identity may live in this database's `identity` schema or in a
// separate database entirely (TSM_IDENTITY_DATABASE_*), and Postgres has no
// cross-database foreign keys. A grant therefore outlives the user it names, and
// something has to be able to tell an administrator from a leftover row. That
// something is this, on the identity connection, which is exactly the reach an
// FK would not have had.
type identityResolver struct {
	users *idstore.UserRepository
}

// UserExists reports whether userID still resolves to an identity user.
//
// THREE OUTCOMES, AND THE THIRD IS THE POINT. (true, nil) and (false, nil) are
// answers; (false, err) is "I could not find out". The carrier's floor
// predicate treats the third as fatal rather than as "this grant does not
// count", because an identity store that is down would otherwise report every
// remaining grant as an orphan and let the last real administrator revoke
// themselves during exactly the incident in which nobody can be added back.
// Collapsing the error into false is the single defect this signature exists to
// make unwriteable.
//
// The empty id answers (false, nil) WITHOUT querying: user_id is UUID, so an
// empty string reaches Postgres as an invalid-input error, and "no principal" is
// a clean no rather than a database fault.
//
// Platform-wide tenancy (OrgScopeAllOrganizations) deliberately, and for the
// same reason middleware.AuthMiddleware resolves a token's subject that way:
// this IS authority derivation. There is no caller tenancy to scope by — the
// question is whether a platform-wide grant names a live principal, and scoping
// it to some organization would make a grant look orphaned merely because the
// user belongs somewhere the scope did not reach.
func (r identityResolver) UserExists(ctx context.Context, userID string) (bool, error) {
	if r.users == nil {
		return false, fmt.Errorf("%w: no identity user repository is wired, so no grant can be shown "+
			"to name a live principal", idplatformadmin.ErrIdentityUnavailable)
	}
	if strings.TrimSpace(userID) == "" {
		return false, nil
	}
	user, err := r.users.GetUserByID(ctx, userID, idstore.OrgScopeAllOrganizations())
	if errors.Is(err, idstore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return user != nil, nil
}
