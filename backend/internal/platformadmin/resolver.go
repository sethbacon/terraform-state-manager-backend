package platformadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
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
	user, err := r.lookup(ctx, userID)
	if err != nil {
		return false, err
	}
	return user != nil, nil
}

// lookup resolves userID to the identity user behind it, or to (nil, nil) when
// nobody answers to that id.
//
// It is UserExists with the person kept rather than discarded, and it is the
// only reader of the identity store in this package — the existence question and
// the "who is this?" question are the same query, and answering them separately
// would be two round trips per grant that could disagree with each other.
//
// The three-outcome contract UserExists documents is this function's: (user,
// nil) and (nil, nil) are answers, (nil, err) is "I could not find out", and
// collapsing the third into the second is the defect both signatures exist to
// make unwriteable.
func (r identityResolver) lookup(ctx context.Context, userID string) (*idmodels.User, error) {
	if r.users == nil {
		return nil, fmt.Errorf("%w: no identity user repository is wired, so no grant can be shown "+
			"to name a live principal", idplatformadmin.ErrIdentityUnavailable)
	}
	if strings.TrimSpace(userID) == "" {
		return nil, nil
	}
	user, err := r.users.GetUserByID(ctx, userID, idstore.OrgScopeAllOrganizations())
	if errors.Is(err, idstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return user, nil
}
