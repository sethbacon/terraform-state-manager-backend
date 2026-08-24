package repositories

import (
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

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

// ---------------------------------------------------------------------------
// The PAGED scoped readers (#459)
//
// ListSources serves a page and a total together, and the two have to agree
// about which rows exist. A scoped page beside an unscoped COUNT reports
// "3 of 400" to a tenant who owns three, and the number that leaks is exactly
// the one the partition hides — so both are covered here, and the empty-scope
// case asserts that NEITHER touches the database.
// ---------------------------------------------------------------------------

func TestSourceRepository_PagedScopedReadsOnAnEmptyScopeTouchNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	out, err := r.ListPageInScope(ctx, tenantscope.Scope{}, 50, 0)
	if err != nil {
		t.Fatalf("ListPageInScope on an empty scope: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("ListPageInScope on an empty scope returned %d sources; it must return none", len(out))
	}

	n, err := r.CountInScope(ctx, tenantscope.Scope{})
	if err != nil {
		t.Fatalf("CountInScope on an empty scope: %v", err)
	}
	if n != 0 {
		t.Errorf("CountInScope on an empty scope returned %d; a caller whose tenancy could not "+
			"be established is told the store is empty, not told its size", n)
	}

	// No expectations registered, so any statement at all fails here.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSourceRepository_ListPageInScope_FiltersAndPages(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA, scopeOrgB}}

	// The organization array is $1, so LIMIT and OFFSET are $2 and $3. If that
	// order is ever reversed, this expectation is what says so — and a page
	// bound to the wrong parameters is a page of somebody else's rows.
	mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{scopeOrgA, scopeOrgB}, 2, 4).
		WillReturnRows(sourceRow())

	out, err := r.ListPageInScope(ctx, scope, 2, 4)
	if err != nil {
		t.Fatalf("ListPageInScope: %v", err)
	}
	if len(out) != 1 || out[0].ID != "s1" {
		t.Errorf("unexpected sources: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	mock.ExpectQuery("FROM state_sources WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := r.ListPageInScope(ctx, scope, 2, 4); err == nil {
		t.Error("ListPageInScope swallowed the query error")
	}
}

func TestSourceRepository_CountInScope_FiltersByOrganization(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA}}

	mock.ExpectQuery("SELECT count.+FROM state_sources WHERE organization_id = ANY").
		WithArgs([]string{scopeOrgA}).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	n, err := r.CountInScope(ctx, scope)
	if err != nil {
		t.Fatalf("CountInScope: %v", err)
	}
	if n != 3 {
		t.Errorf("CountInScope = %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	mock.ExpectQuery("SELECT count").WillReturnError(errDB)
	if _, err := r.CountInScope(ctx, scope); err == nil {
		t.Error("CountInScope swallowed the query error")
	}
}

// A platform administrator reaches every organization, so neither paged reader
// may send them through the organization predicate — doing so would filter the
// one principal that is genuinely deployment-wide down to a membership list
// they may not even have.
func TestSourceRepository_PagedScopedReads_PlatformAdminIsUnfiltered(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)
	admin := tenantscope.Scope{PlatformAdmin: true}

	// No WHERE, and no organization argument bound.
	mock.ExpectQuery("FROM state_sources ORDER BY created_at DESC").
		WithArgs(10, 0).
		WillReturnRows(sourceRow())
	if _, err := r.ListPageInScope(ctx, admin, 10, 0); err != nil {
		t.Fatalf("ListPageInScope for a platform admin: %v", err)
	}

	mock.ExpectQuery("SELECT count\\(\\*\\) FROM state_sources$").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(9))
	n, err := r.CountInScope(ctx, admin)
	if err != nil {
		t.Fatalf("CountInScope for a platform admin: %v", err)
	}
	if n != 9 {
		t.Errorf("CountInScope = %d, want the unfiltered 9", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
