package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenancy"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// The authority a dispatch chain runs under, and the scoped loads it makes.
//
// Written once because the last version of this rule was written twice and only
// one copy was right: health.CreateRun compared organizations and refused, while
// dispatchDrift loaded the connection by id and handed it straight to
// resolvePipelineToken, which DECRYPTS the connection's token or its CI source's
// shared token. So the credential was in memory before anything asked whether
// the caller was entitled to it.
//
// # From comparison to scope (#393 option B)
//
// The first version of this file compared organization ids in Go after an
// UNSCOPED load — ambient, row-derived authority, with an allowance for
// unstamped rows. That model has the exact weakness the #393 decision names:
// each new load on the chain needs its own remembered comparison, and the one
// that was forgotten (resolvePipelineToken's ci_sources hop) let a connection in
// one organization decrypt another organization's SHARED CI credential.
//
// Now the chain carries ONE dispatchAuthority — a real tenantscope.Scope for
// exactly one organization, with provenance — and every by-id load under it is
// an InScope read. A row in another organization matches no row, so a chain that
// crosses organizations fails closed in SQL rather than depending on a
// comparison somebody has to keep writing. The unstamped-row allowance is gone
// with it: post-000034 the schema cannot produce such a row, a restored
// pre-000034 backup can, and a row that belongs to no organization is dispatched
// by no one until the backfill stamps it (the same fail-closed answer every
// scoped reader gives).

// dispatchAuthority is the single-organization authority every load on a
// dispatch chain is scoped by, and it records where that authority came from.
//
// PROVENANCE IS THE AUDITABLE HALF of the #393 decision: a refusal or log line
// built from this value can always say whether the scope was resolved from a
// request or derived by the system, and from which row. The two constructors
// below are the only ways to build one, so an origin string is never absent and
// the two kinds can never be conflated.
type dispatchAuthority struct {
	scope          tenantscope.Scope
	organizationID string
	// origin is "request" for a request-resolved acting organization, or the
	// SystemScope's "system:<table>/<id>" for a derived one.
	origin string
	// system reports a system-derived authority, distinguishable wherever the
	// value is consumed — the provenance property, as a field rather than a
	// string-prefix convention.
	system bool
}

// requestAuthority scopes a dispatch to the acting organization a request
// resolved (actingOrganization has already verified it against the caller's
// tenant scope).
func requestAuthority(organizationID string) dispatchAuthority {
	return dispatchAuthority{
		scope:          tenantscope.Scope{OrgIDs: []string{organizationID}},
		organizationID: organizationID,
		origin:         "request",
	}
}

// systemAuthority scopes a dispatch to the organization a tenancy.SystemScope
// derived from the row being processed. The zero SystemScope yields an
// authority whose scope permits nothing, so a caller that forgets to derive
// reads nothing rather than everything.
func systemAuthority(s tenancy.SystemScope) dispatchAuthority {
	return dispatchAuthority{
		scope:          s.Scope(),
		organizationID: s.OrganizationID(),
		origin:         s.Origin(),
		system:         true,
	}
}

// errNotOwnedHere reports a target the dispatch authority's organization does
// not own. It is deliberately mapped to "not found" by every caller: a caller
// outside the owning organization must not be able to tell a target that exists
// elsewhere from one that does not exist at all.
var errNotOwnedHere = errors.New("api: the target is not reachable in the dispatching organization")

// pipelineConnectionFor loads a pipeline connection under the dispatch
// authority's scope. A connection in another organization — or one with no
// organization at all — matches no row and returns (nil, nil), exactly like a
// connection that does not exist.
func pipelineConnectionFor(
	ctx context.Context,
	repo *repositories.PipelineRepository,
	id string, auth dispatchAuthority,
) (*repositories.PipelineConnection, error) {
	return repo.GetByIDInScope(ctx, id, auth.scope)
}

// sourceFor is the same rule for a dispatch target's state source. A drift
// target names one, and it decides which source's state a CI job is pointed at.
func sourceFor(
	ctx context.Context,
	repo *repositories.SourceRepository,
	id string, auth dispatchAuthority,
) (*repositories.Source, error) {
	if id == "" {
		return nil, nil
	}
	src, err := repo.GetByIDInScope(ctx, id, auth.scope)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, errNotOwnedHere
	}
	return src, nil
}

// organizationScope returns a scope permitting exactly organizationID, or the
// empty scope -- which every InScope reader treats as "read nothing, without a
// query" -- when the id is blank. The guard against a blank id is load-bearing:
// binding [""] into a uuid[] would be a Postgres type error, and treating a row
// with no organization as unable to reference anything is the fail-closed
// reading of an unstamped row.
func organizationScope(organizationID string) tenantscope.Scope {
	if strings.TrimSpace(organizationID) == "" {
		return tenantscope.Scope{}
	}
	return tenantscope.Scope{OrgIDs: []string{organizationID}}
}

// errChainCrossesOrganizations makes a refused chained load OBSERVABLE with its
// provenance, which is the difference between a poisoned row an operator can
// find and a silent skip nobody ever will. The wrapped sentinel keeps the HTTP
// mapping intact (handlers render their own fixed 404/500 messages, never this
// text), while a scheduler log line carries the whole story: which load was
// refused, under which authority, derived from which row.
func errChainCrossesOrganizations(table, id string, auth dispatchAuthority, sentinel error) error {
	return fmt.Errorf(
		"%w: %s/%s is not reachable under %s (organization %s) — the row is gone, unstamped, or belongs to another organization",
		sentinel, table, id, auth.origin, auth.organizationID)
}
