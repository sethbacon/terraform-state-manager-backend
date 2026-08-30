// systemscope.go is the background-authority half of this package: the
// constructor for "the system, acting in organization X", decided on #393
// (option B, 2026-08-29).
//
// # Why this exists
//
// Background machinery — the scheduler today; the health runner, drift
// reconciler and notifier as their increments land — has no request and
// therefore no principal, so nothing upstream can resolve a tenant scope for
// it. The three candidate answers were: ambient row-derived authority (compare
// organization ids in Go at each site, with a growing exemption list), a
// privileged worker (a synthetic platform admin — the unscoped read #393 exists
// to eliminate, institutionalized), and this: a REAL scope for exactly one
// organization, derived from the row being processed and passed through the
// SAME InScope readers the request path uses.
//
// The difference from ambient authority is enforcement, not information. Both
// read the organization off the owning row; only this one pushes it through the
// same SQL predicate every request-path read uses, so a chained load
// (schedule -> pipeline_connection -> ci_source) FAILS CLOSED when the chain
// crosses organizations instead of silently succeeding. That chain is not
// hypothetical: the schedules flip found an execution path where one
// organization could fire another's pipeline and decrypt its CI token.
//
// # Where this lives, and why
//
// The shared identity/tenantscope package needed no change: its Scope already
// expresses "exactly one organization" as {OrgIDs: [X]}, and Scope is aliased
// into this application unchanged, so a value built here is type-identical to
// one the request middleware resolves. What the shared type cannot carry is
// PROVENANCE — and provenance is app policy (which rows may confer authority on
// the system is a fact about THIS application's partition), so per
// sethbacon/terraform-suite-identity#206 it belongs app-side. It sits in
// internal/tenancy rather than internal/tenantscope because everything in the
// latter is request-shaped (gin contexts, principals, headers) and a background
// worker importing an HTTP adapter to obtain authority would have the
// dependency arrow pointing the wrong way.
//
// # Provenance is load-bearing, not decorative
//
// A SystemScope is a distinct type from tenantscope.Scope, so a consumer or a
// log line can always tell a system-derived authority from a request-resolved
// one: the request path never produces this type, and this type's Origin()
// always names the row the authority was derived from. That is the auditable
// trail the decision requires — when a background load is refused, the refusal
// can say WHICH row led there, which is the difference between a poisoned row
// an operator can find and a silent skip nobody ever will.
package tenancy

import (
	"errors"
	"fmt"
	"strings"

	idtenantscope "github.com/sethbacon/terraform-suite-identity/identity/tenantscope"
)

// ErrSystemScopeUnowned is returned when the owning row carries no
// organization. Post-000034 the schema cannot produce such a row, but a
// database restored from a pre-000034 backup can, and this is the layer that
// has to keep working when the constraint above it is absent. A row that
// belongs to no organization confers authority over none — deriving a scope
// from it must refuse, loudly, rather than default to anything.
var ErrSystemScopeUnowned = errors.New(
	"tenancy: cannot derive a system scope from a row with no owning organization")

// SystemScope is "the system, acting in organization X", where X came from the
// row being processed.
//
// The zero value derives from nothing, permits nothing (Scope() returns the
// zero tenantscope.Scope, which every InScope reader treats as fail-closed) and
// reports IsZero. Only SystemActingIn produces a usable value, which is what
// makes the provenance mandatory rather than conventional.
type SystemScope struct {
	organizationID string
	origin         string
}

// SystemActingIn derives the system's authority to act in organizationID from
// the row rowTable/rowID — the schedule being fired, the run being reconciled.
//
// The organization ALWAYS comes from the owning row, never from configuration
// or a default: a worker that could name its own organization would be the
// privileged worker option B rejected. The row coordinates are required for the
// same reason the organization is — an authority nobody can trace to a row is
// ambient authority with better manners.
func SystemActingIn(organizationID, rowTable, rowID string) (SystemScope, error) {
	organizationID = strings.TrimSpace(organizationID)
	rowTable = strings.TrimSpace(rowTable)
	rowID = strings.TrimSpace(rowID)
	if rowTable == "" || rowID == "" {
		return SystemScope{}, fmt.Errorf(
			"tenancy: a system scope must name the row it derives from (got table %q, id %q)",
			rowTable, rowID)
	}
	if organizationID == "" {
		return SystemScope{}, fmt.Errorf("%w (row %s/%s)", ErrSystemScopeUnowned, rowTable, rowID)
	}
	return SystemScope{organizationID: organizationID, origin: rowTable + "/" + rowID}, nil
}

// Scope returns the derived authority as the same Scope type every InScope
// reader takes, permitting exactly the one organization the owning row named.
// The zero SystemScope yields the zero Scope, which permits nothing.
func (s SystemScope) Scope() idtenantscope.Scope {
	if s.organizationID == "" {
		return idtenantscope.Scope{}
	}
	return idtenantscope.Scope{OrgIDs: []string{s.organizationID}}
}

// OrganizationID is the one organization this authority reaches — the value a
// row created under it must be stamped with.
func (s SystemScope) OrganizationID() string { return s.organizationID }

// Origin names the row this authority was derived from, prefixed "system:" so
// that no request-resolved provenance string can collide with it. Empty for the
// zero value.
func (s SystemScope) Origin() string {
	if s.origin == "" {
		return ""
	}
	return "system:" + s.origin
}

// IsZero reports a SystemScope that was never derived. Consumers refuse it; the
// zero Scope it would yield already reads nothing, but refusing early keeps the
// failure at the seam that caused it rather than surfacing as an empty result.
func (s SystemScope) IsZero() bool { return s.organizationID == "" }

// String makes log lines self-describing without callers assembling the parts.
func (s SystemScope) String() string {
	if s.IsZero() {
		return "system scope (underived; permits nothing)"
	}
	return fmt.Sprintf("system scope in organization %s derived from %s", s.organizationID, s.origin)
}
