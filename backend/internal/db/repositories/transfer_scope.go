// transfer_scope.go is the Phase 3 read flip for state_transfers (#393, tracking
// #502) — the ninth and last partition root.
//
// # A transfer names two organizations and the row records one, ON PURPOSE
//
// 000033 keeps a transfer whose ends sit in different organizations as a
// SUPPORTED way to move a state file across the boundary this partition draws,
// and the write path already enforces the only condition that makes that safe:
// doTransfer loads BOTH ends through SourceRepository.GetByIDInScope under the
// caller's resolved scope — the target BEFORE its credentials are decrypted, so
// a refusal is not a late one — and transferEndpointsReachable refuses a caller
// who cannot reach either end. The row is then stamped with the CALLER'S acting
// organization, because the transfer is an act and this records whose act it
// was, and PR #541 gives the counterparty organization its own audit entry so a
// move out of it is not invisible to it.
//
// SO THE READ BELOW SCOPES TO THAT ONE ORGANIZATION AND MUST NOT DO ANYTHING
// CLEVERER. Making a transfer visible to both ends through this predicate was
// considered and is wrong in both directions:
//
//   - joining up to state_sources to admit either end would show one organization
//     the other's source ids and state keys, which are exactly the fields a
//     transfer record consists of;
//   - deriving the organization from the source instead of the caller would hide
//     a transfer from the organization that performed it the moment the ends
//     disagree.
//
// The counterparty's need is to KNOW, and that need is met by the audit entry.
// It is not met by widening a tenant predicate, and conflating the two would
// quietly re-open the boundary this root is the last one to close.
//
// # There is no list read on this root, and that is not an omission
//
// TransferRepository exposes Create and GetByID and nothing else; the only route
// that reads it is GET /transfers/{id}, served the id returned by the transfer
// that created the row. So the whole read surface of state_transfers is the one
// by-id read scoped here. If a history or list endpoint is ever added it needs a
// ListInScope alongside it, and unscoped_twin_class_test.go will say so the
// moment an unscoped List exists to call.
package repositories

import (
	"context"
	"database/sql"
	"errors"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// GetByIDInScope returns one transfer when the scope permits it, and (nil, nil)
// otherwise.
//
// A transfer in another organization is reported EXACTLY as one that does not
// exist. The record names both source ids, both state keys and the actor, so
// "that one is not yours" would both disclose that the transfer happened and let
// a caller enumerate ids.
//
// An empty scope reads nothing WITHOUT ISSUING A QUERY. That is not an
// optimisation: a query with an empty id list happens to match nothing today,
// which is a fact about how PostgreSQL evaluates `= ANY('{}')` rather than a
// decision this repository made, and the decision is what has to survive an edit.
func (r *TransferRepository) GetByIDInScope(ctx context.Context, id string, scope tenantscope.Scope) (*Transfer, error) {
	if scope.Empty() {
		return nil, nil
	}
	if scope.PlatformAdmin {
		// Unfiltered, so a row whose organization_id is still NULL — written by
		// a replica on the previous build, before the boot backfill — stays
		// reachable by the one caller who can repair it. The tenant predicate
		// cannot match it: `NULL = ANY(...)` is NULL, never true.
		return r.GetByID(ctx, id)
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+transferColumns+` FROM state_transfers
		  WHERE organization_id = ANY($1::uuid[]) AND id = $2`, scope.OrgIDs, id)
	t, err := scanTransfer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}
