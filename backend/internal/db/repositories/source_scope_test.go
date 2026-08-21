package repositories

import (
	"database/sql"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// ---------------------------------------------------------------------------
// SourceRepository — organization-scoped reads (#393 Phase 2b)
//
// These cover the shape of the query. Whether the shape returns the RIGHT ROWS
// is a question about PostgreSQL and is answered against a real one in
// internal/tenancy/scoped_read_integration_test.go — a mock cannot tell you that
// `NULL = ANY(...)` is not true, and that is the property the whole partition
// rests on.
//
// MUTATION-VERIFIED. Each was run against a deliberately broken repository:
//
//	Empty() short-circuit removed        -> TestSourceRepository_ScopedReadsOnAnEmptyScope...
//	predicate dropped from ListInScope   -> TestSourceRepository_ListInScope_FiltersByOrganization
//	predicate dropped from GetByIDInScope-> TestSourceRepository_GetByIDInScope_FiltersByOrganization
//	platform admin sent through the      -> TestSourceRepository_ScopedReads_PlatformAdminIsUnfiltered
//	  organization predicate
// ---------------------------------------------------------------------------

const (
	scopeOrgA = "11111111-1111-4111-8111-111111111111"
	scopeOrgB = "22222222-2222-4222-8222-222222222222"
)

// THE FAIL-CLOSED CASE, and it is asserted as "no statement reached the
// database" rather than as "no rows came back". Those are different claims: a
// reader that issued the query and happened to match nothing would satisfy the
// second while still being one predicate edit away from returning the whole
// table. A caller whose tenancy could not be established has no business
// SELECTing from state_sources at all.
func TestSourceRepository_ScopedReadsOnAnEmptyScopeTouchNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	out, err := r.ListInScope(ctx, tenantscope.Scope{})
	if err != nil {
		t.Fatalf("ListInScope on an empty scope: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("ListInScope on an empty scope returned %d sources; it must return none", len(out))
	}

	got, err := r.GetByIDInScope(ctx, "s1", tenantscope.Scope{})
	if err != nil {
		t.Fatalf("GetByIDInScope on an empty scope: %v", err)
	}
	if got != nil {
		t.Errorf("GetByIDInScope on an empty scope returned %+v; it must return nothing", got)
	}

	// No expectations were registered, so any statement at all fails here.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSourceRepository_ListInScope_FiltersByOrganization(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA, scopeOrgB}}
	mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{scopeOrgA, scopeOrgB}).
		WillReturnRows(sourceRow())

	out, err := r.ListInScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	if len(out) != 1 || out[0].ID != "s1" {
		t.Errorf("unexpected sources: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := r.ListInScope(ctx, scope); err == nil {
		t.Error("ListInScope swallowed the query error")
	}
}

func TestSourceRepository_GetByIDInScope_FiltersByOrganization(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA}}

	// The organization array is $1 and the id is $2, so both readers share one
	// predicate string. If that order is ever reversed, this expectation is what
	// says so.
	mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{scopeOrgA}, "s1").
		WillReturnRows(sourceRow())
	got, err := r.GetByIDInScope(ctx, "s1", scope)
	if err != nil || got == nil || got.ID != "s1" {
		t.Fatalf("GetByIDInScope: %v %+v", err, got)
	}

	// A row outside the scope is reported EXACTLY as a row that does not exist.
	// Anything else would let a caller enumerate ids and learn which of them
	// name real sources elsewhere in the deployment.
	mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{scopeOrgA}, "elsewhere").
		WillReturnError(sql.ErrNoRows)
	got, err = r.GetByIDInScope(ctx, "elsewhere", scope)
	if err != nil || got != nil {
		t.Errorf("a source outside the scope must be (nil, nil), got %+v %v", got, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A platform admin reads unfiltered — TSM's only principal that is not somebody's
// tenant admin (tenantscope.Scope.PlatformAdmin). Asserted as "the statement
// carried no arguments", which is exactly what distinguishes the unscoped query
// from the scoped one: the scoped query always binds the organization array.
func TestSourceRepository_ScopedReads_PlatformAdminIsUnfiltered(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	admin := tenantscope.Scope{PlatformAdmin: true}

	mock.ExpectQuery("FROM state_sources ORDER BY created_at DESC").
		WithArgs().
		WillReturnRows(sourceRow())
	if _, err := r.ListInScope(ctx, admin); err != nil {
		t.Fatalf("ListInScope(platform admin): %v", err)
	}

	mock.ExpectQuery("FROM state_sources WHERE id").
		WithArgs("s1").
		WillReturnRows(sourceRow())
	if _, err := r.GetByIDInScope(ctx, "s1", admin); err != nil {
		t.Fatalf("GetByIDInScope(platform admin): %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
