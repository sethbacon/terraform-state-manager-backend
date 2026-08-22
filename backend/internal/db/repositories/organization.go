package repositories

import "errors"

// ErrNoOrganization is returned by a Create that was given no owning
// organization.
//
// It exists as a named, shared sentinel rather than a per-repository error
// string because #436 is a NINE-TABLE class, not one bug: every partition root
// has the same refusal, and a caller that wants to distinguish "you did not say
// who owns this" from a database failure should be able to, once, for all of
// them.
//
// Refusing is the only safe answer. Omitting organization_id from an INSERT
// falls through to the column DEFAULT — which is what put every row in the
// deployment in the default organization — and naming it with an empty value
// writes NULL, which is invisible to every tenant. Neither failure is visible at
// the call site; this one is.
var ErrNoOrganization = errors.New("repositories: no owning organization was supplied")
