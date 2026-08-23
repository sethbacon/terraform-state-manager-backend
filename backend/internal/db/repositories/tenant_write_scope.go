package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// Scoping the DESTRUCTIVE side of the organization partition.
//
// #436 stamped every INSERT, and the read side is being scoped separately. The
// third side went unexamined: every UPDATE and DELETE on a partition root found
// its row BY ID ALONE.
//
// With one organization that is invisible. With two it means a caller holding
// sources:manage in organization B can delete organization A's state source by
// id -- and state_sources cascades to state_backups, state_edits, state_locks,
// state_analyses, source_sync_status, state_analysis_history, state_module_refs
// and state_transfers. That is cross-tenant destruction, not a disclosure, and
// it is the one class here that cannot be undone by fixing the code afterwards.
//
// # Why a write predicate is safe to land before the estate is re-owned
//
// A scoped READ shows a tenant nothing until their rows have been moved to them,
// so flipping reads early is a visible regression. A scoped WRITE has no such
// window: a caller can only reach rows their organization owns, and a caller
// whose organization owns nothing could not legitimately write anything anyway.
// Both before and after `reown-roots move`, the answer is the one the data
// supports.
//
// # Zero rows affected is a 404, never a 403
//
// A row that exists in another organization is reported EXACTLY as a row that
// does not exist, for the reason GetByIDInScope already gives: answering "that
// one is not yours" lets a caller enumerate ids and learn which of them name
// real rows somewhere in the deployment.

// writeOrgPredicate is the organization filter for a mutating statement.
//
// It excludes NULL for the same reason the read predicate does: `NULL = ANY(...)`
// is NULL and never true, so an unstamped row is unreachable rather than
// reachable-by-everyone. On the write side that is the conservative direction --
// an unstamped row cannot be destroyed by a tenant, and the boot backfill will
// stamp it.
const writeOrgPredicate = `organization_id = ANY($%d::uuid[])`

// tenantWrite describes how a mutating statement should be scoped.
type tenantWrite struct {
	// Skip is true when the scope reaches everything (a platform admin), so the
	// caller should run its original unscoped statement.
	Skip bool
	// Deny is true when the scope reaches nothing, so the caller must not run a
	// statement at all.
	Deny bool
	// OrgIDs is the bound organization list when neither of the above holds.
	OrgIDs []string
}

// scopeWrite folds a Scope into the three outcomes a mutating statement has.
//
// PLATFORM ADMIN RUNS THE UNSCOPED STATEMENT, deliberately. It must be able to
// reach rows whose organization_id is still NULL -- written by a replica on the
// previous build, before the backfill -- which the predicate cannot match. That
// is the same carve-out ListInScope makes, and for the same reason.
func scopeWrite(scope tenantscope.Scope) tenantWrite {
	if scope.PlatformAdmin {
		return tenantWrite{Skip: true}
	}
	if scope.Empty() {
		return tenantWrite{Deny: true}
	}
	return tenantWrite{OrgIDs: scope.OrgIDs}
}

// ErrNotInScope is returned by a scoped mutation whose scope reaches nothing.
//
// Distinct from "no such row": the caller could not have reached ANY row, which
// is a wiring or membership condition rather than a fact about this id. Handlers
// still render it as a 404 -- see the file comment on why a 403 would be a
// disclosure -- but the two are told apart in logs.
var ErrNotInScope = errors.New("repositories: the caller's scope reaches no organization")

// DeleteInScope removes a source only when the caller's organization owns it.
//
// Returns (false, nil) when no row matched, which covers BOTH "no such id" and
// "that id belongs to another organization". The caller cannot distinguish them
// and must not be able to.
func (r *SourceRepository) DeleteInScope(ctx context.Context, id string, scope tenantscope.Scope) (bool, error) {
	w := scopeWrite(scope)
	if w.Deny {
		return false, ErrNotInScope
	}
	if w.Skip {
		if err := r.Delete(ctx, id); err != nil {
			return false, err
		}
		return true, nil
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM state_sources WHERE id = $1 AND organization_id = ANY($2::uuid[])`, id, w.OrgIDs)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// A driver that cannot report the row count cannot tell us whether the
		// delete applied. Report it rather than guessing "yes": the caller uses
		// this to decide between 204 and 404.
		return false, err
	}
	return n > 0, nil
}

// UpdateInScope updates a source only when the caller's organization owns it.
//
// Returns (nil, nil) when no row matched — the same shape Update already uses
// for a missing row, so a row in another organization and a row that does not
// exist reach the handler identically.
func (r *SourceRepository) UpdateInScope(ctx context.Context, s *Source, scope tenantscope.Scope) (*Source, error) {
	w := scopeWrite(scope)
	if w.Deny {
		return nil, ErrNotInScope
	}
	if w.Skip {
		return r.Update(ctx, s)
	}
	configJSON, err := json.Marshal(orEmptyMap(s.Config))
	if err != nil {
		return nil, err
	}
	scopeJSON, err := json.Marshal(orEmptyMap(s.Scope))
	if err != nil {
		return nil, err
	}
	var endpoint any
	if s.Endpoint != "" {
		endpoint = s.Endpoint
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE state_sources SET
			name = $2,
			endpoint = $3,
			config = $4::jsonb,
			scope = $5::jsonb,
			encrypted_credentials = CASE WHEN $6::bytea IS NULL THEN encrypted_credentials ELSE $6 END,
			updated_at = now()
		WHERE id = $1 AND organization_id = ANY($7::uuid[])
		RETURNING `+sourceColumns,
		s.ID, s.Name, endpoint, string(configJSON), string(scopeJSON),
		nullableBytes(s.EncryptedCredentials), w.OrgIDs)
	updated, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update source: %w", err)
	}
	return updated, nil
}
