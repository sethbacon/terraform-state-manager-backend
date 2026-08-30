package api

import (
	"crypto/subtle"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
)

// The authority a MACHINE CALLBACK acts under — #393 option B, item 5.
//
// # There is no principal here, and inventing one would be the bug
//
// A drift or health run is dispatched to somebody's CI, and the CI job posts its
// result back to /drift/runs/:id/results carrying one thing: the per-run
// callback token TSM generated at dispatch. No session, no API key, no
// membership, no organization header. `tenantscope.Resolve` returns the empty
// scope for a request like that, correctly — the empty scope is the honest
// answer to a question with no subject — and middleware.TenantScope on this
// route would refuse every legitimate callback the product depends on.
//
// The two wrong answers are worth naming because both look reasonable at the
// call site. Running the callback unscoped is the privileged worker #393 exists
// to eliminate, and it is what shipped: a token authenticated one run and every
// statement afterwards addressed rows by id alone. Giving the callback a
// platform-admin scope is worse — it is the same thing wearing the one carrier
// that is supposed to mean a live-checked human administrator, and it takes the
// bypass branch of every InScope reader in the process.
//
// # The credential IS the authority
//
// The run row carries an organization_id of its own (000033: both of a drift
// run's parents are ON DELETE SET NULL, so an inherited answer would be NULL for
// exactly the rows that outlive their source). So the chain is short and has no
// resolution step in it: authenticate the token, which identifies the run; the
// run names the organization; that organization derives a tenancy.SystemScope
// with the run as its provenance; every statement afterwards is InScope under
// it. Same constructor, same Scope type and same predicates as the scheduler
// path, which is what keeps this from being a second authority model.
//
// # What the failure direction has to be
//
// A token that does not authenticate must yield NO authority — not a narrower
// one, not the empty-but-usable one. That is why this returns a bool rather than
// an error the caller might log and continue past, and why the zero
// dispatchAuthority it returns alongside false carries the zero Scope, which
// every InScope reader treats as "read nothing, without a query". A caller that
// ignored the bool would still reach no row.

// callbackRun is the subset of an authenticated run this derivation needs: the
// row's identity, its organization, and the token it was dispatched with.
//
// A struct rather than three loose strings so a caller cannot transpose the
// stored token and the presented one — which would compare a value against
// itself and authenticate anything.
type callbackRun struct {
	// ID is the run row's id, and becomes the derived authority's provenance.
	ID string
	// OrganizationID is the run's own organization column.
	OrganizationID string
	// StoredToken is the token column as read from the row. Empty means the
	// callback has already been consumed (or the run was expired by the
	// reconciler), and empty must never authenticate.
	StoredToken string
}

// authenticateCallback compares presented against the run's stored token in
// constant time and, only on a match, derives the system authority that
// organization confers.
//
// rowTable is the run's table ("drift_runs" / "health_runs") and appears in the
// authority's origin, so a refused load downstream can name the row the
// authority came from rather than reporting an anonymous denial.
//
// Reports false — with an authority that permits nothing — when the presented
// token is empty, when the stored token is empty, when they differ, or when the
// run belongs to no organization. That last case is not hypothetical
// housekeeping: a database restored from a backup taken before 000034 holds
// unstamped rows, and a run that belongs to no organization confers authority
// over none. Deriving "everything" from it is precisely the unpartitioned read
// this whole issue is about.
func authenticateCallback(rowTable string, run callbackRun, presented string) (dispatchAuthority, bool) {
	// Both empties are rejected before the compare rather than relying on it:
	// ConstantTimeCompare("", "") returns 1, so a run whose token has already
	// been consumed would authenticate a caller who sent no token at all.
	if presented == "" || run.StoredToken == "" {
		return dispatchAuthority{}, false
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(run.StoredToken)) != 1 {
		return dispatchAuthority{}, false
	}
	sys, err := tenancy.SystemActingIn(run.OrganizationID, rowTable, run.ID)
	if err != nil {
		return dispatchAuthority{}, false
	}
	return systemAuthority(sys), true
}

// callbackTokenFrom returns the token a callback presented: the header, or the
// body field for runners that cannot set one.
//
// Extracted so both callbacks read it the same way, and so the precedence is
// stated once — the header wins, and a blank header falls through to the body
// rather than authenticating as an empty token.
func callbackTokenFrom(header, body string) string {
	if h := strings.TrimSpace(header); h != "" {
		return h
	}
	return body
}
