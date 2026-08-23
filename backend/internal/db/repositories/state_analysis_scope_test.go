package repositories

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// Unit cover for the two branches of every scoped reader that need no database:
// an empty scope reads nothing, and a platform admin delegates to the unscoped
// reader.
//
// The SCOPED path itself is covered against a real PostgreSQL in
// state_analysis_scope_integration_test.go, because its tenant predicate is a
// JOIN and a mock cannot evaluate one — it returns whatever the fixture declares
// regardless of what the join would have excluded. What is asserted here is the
// control flow around it, which is ordinary Go.

func scopeNone() tenantscope.Scope  { return tenantscope.Scope{} }
func scopeAdmin() tenantscope.Scope { return tenantscope.Scope{PlatformAdmin: true} }
func scopeOne() tenantscope.Scope {
	return tenantscope.Scope{OrgIDs: []string{"aaaaaaaa-0000-4000-8000-000000000001"}}
}

// TestScopedReaders_EmptyScopeQueriesNothing. The early return is what makes
// "reads nothing" a statement in the code rather than a consequence of how
// `= ANY('{}')` happens to evaluate. Registering no expectations means any query
// at all is a failure.
func TestScopedReaders_EmptyScopeQueriesNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)
	ctx := context.Background()

	if got, err := r.TotalsInScope(ctx, scopeNone()); err != nil || got.States != 0 {
		t.Errorf("TotalsInScope = %+v, %v; want zeroed totals", got, err)
	}
	if got, err := r.ProviderCountsInScope(ctx, scopeNone()); err != nil || len(got) != 0 {
		t.Errorf("ProviderCountsInScope = %v, %v; want empty", got, err)
	}
	if got, err := r.ResourceTypeCountsInScope(ctx, scopeNone()); err != nil || len(got) != 0 {
		t.Errorf("ResourceTypeCountsInScope = %v, %v; want empty", got, err)
	}
	if got, err := r.VersionCountsInScope(ctx, scopeNone()); err != nil || len(got) != 0 {
		t.Errorf("VersionCountsInScope = %v, %v; want empty", got, err)
	}
	if got, err := r.StateVersionsInScope(ctx, scopeNone()); err != nil || len(got) != 0 {
		t.Errorf("StateVersionsInScope = %v, %v; want empty", got, err)
	}
	if got, total, err := r.StatesByVersionExactInScope(ctx, scopeNone(), "1.9.5", 10); err != nil || len(got) != 0 || total != 0 {
		t.Errorf("StatesByVersionExactInScope = %v/%d, %v; want empty", got, total, err)
	}
	if got, err := r.FilterStatesInScope(ctx, scopeNone(), StateQueryFilter{}); err != nil || len(got) != 0 {
		t.Errorf("FilterStatesInScope = %v, %v; want empty", got, err)
	}
	if got, agg, err := r.PreviewStatesWithTotalsInScope(ctx, scopeNone(), StateQueryFilter{}, 10); err != nil || len(got) != 0 || agg.Matched != 0 {
		t.Errorf("PreviewStatesWithTotalsInScope = %v/%+v, %v; want empty", got, agg, err)
	}
	if got, err := r.SyncStatusesInScope(ctx, scopeNone()); err != nil || len(got) != 0 {
		t.Errorf("SyncStatusesInScope = %v, %v; want empty", got, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an empty scope reached the database: %v", err)
	}
}

// TestScopedReaders_PlatformAdminDelegatesUnfiltered. A platform admin must also
// see rows whose organization_id is still NULL — written by a replica on the
// previous build, before the backfill — which the organization predicate cannot
// match. Delegating to the unscoped reader is how that happens, so the assertion
// is that the UNSCOPED statement runs (it has no `a`-aliased join).
func TestScopedReaders_PlatformAdminDelegatesUnfiltered(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)

	mock.ExpectQuery(`FROM state_analyses\s*$`).
		WillReturnRows(sqlmock.NewRows([]string{"count", "rum", "managed", "data", "total"}).
			AddRow(3, 30, 3, 0, 3))

	got, err := r.TotalsInScope(context.Background(), scopeAdmin())
	if err != nil {
		t.Fatalf("TotalsInScope: %v", err)
	}
	if got.States != 3 {
		t.Errorf("States = %d, want 3", got.States)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a platform admin did not take the unscoped read: %v", err)
	}
}

// TestScopedReaders_BindTheOrganizationArrayFirst. The organization list is $1,
// so buildStateWhere must number the filter's own placeholders from $2. Getting
// that wrong collides on $1 and silently filters by the wrong value — which is
// exactly the kind of thing a fixture-driven mock will happily return rows for,
// so what is asserted is the ARGUMENT, which sqlmock does see.
func TestScopedReaders_BindTheOrganizationArrayFirst(t *testing.T) {
	db, mock := newMock(t)
	r := NewStateAnalysisRepository(db)
	scope := scopeOne()

	mock.ExpectQuery("FROM state_analyses a").
		WithArgs(scope.OrgIDs, "prod").
		WillReturnRows(sqlmock.NewRows([]string{
			"source_id", "name", "type", "state_key", "terraform_version", "serial",
			"lineage", "size", "rum", "managed", "data", "total", "providers",
			"resource_types", "analyzed_at",
		}))

	if _, err := r.FilterStatesInScope(context.Background(), scope,
		StateQueryFilter{KeyContains: "prod"}); err != nil {
		t.Fatalf("FilterStatesInScope: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the organization array was not bound first: %v", err)
	}
}
