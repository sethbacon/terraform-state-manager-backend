package repositories

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// The three outcomes a scoped mutation has, at the repository layer.
//
// The cross-tenant refusal itself is asserted end to end through the handler in
// internal/api/cross_tenant_write_test.go; what is covered here is the control
// flow each outcome takes, and that the organization is actually BOUND — which
// is a thing sqlmock can see, unlike the predicate's effect, which needs a real
// server.

func writeScope() tenantscope.Scope {
	return tenantscope.Scope{OrgIDs: []string{"aaaaaaaa-0000-4000-8000-000000000001"}}
}

// TestDeleteInScope_EmptyScopeRunsNothing. A caller whose tenancy could not be
// established must not reach a DELETE at all. Registering no expectation means
// any statement is a failure.
func TestDeleteInScope_EmptyScopeRunsNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	if _, err := r.DeleteInScope(context.Background(), "s1", tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
		t.Fatalf("error = %v, want ErrNotInScope", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an empty scope reached the database: %v", err)
	}
}

func TestUpdateInScope_EmptyScopeRunsNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	if _, err := r.UpdateInScope(context.Background(), &Source{ID: "s1"}, tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
		t.Fatalf("error = %v, want ErrNotInScope", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an empty scope reached the database: %v", err)
	}
}

// TestDeleteInScope_PlatformAdminRunsTheUnscopedStatement. A platform admin must
// also reach rows whose organization_id is still NULL — written by a replica on
// the previous build, before the backfill — which the predicate cannot match.
func TestDeleteInScope_PlatformAdminRunsTheUnscopedStatement(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	// One argument, not two: the unscoped statement binds only the id.
	mock.ExpectExec("DELETE FROM state_sources").WithArgs("s1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := r.DeleteInScope(context.Background(), "s1", tenantscope.Scope{PlatformAdmin: true})
	if err != nil || !ok {
		t.Fatalf("DeleteInScope = %v, %v; want true, nil", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a platform admin did not take the unscoped statement: %v", err)
	}
}

// TestDeleteInScope_ReportsWhetherARowMatched is what lets the handler tell 204
// from 404. A delete that matched nothing must not be reported as success.
func TestDeleteInScope_ReportsWhetherARowMatched(t *testing.T) {
	for _, tc := range []struct {
		name     string
		affected int64
		want     bool
	}{
		{"row matched", 1, true},
		{"nothing matched (wrong id, or another organization's)", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newMock(t)
			r := NewSourceRepository(db)
			mock.ExpectExec(`DELETE FROM state_sources[\s\S]*organization_id`).
				WithArgs("s1", writeScope().OrgIDs).
				WillReturnResult(sqlmock.NewResult(0, tc.affected))

			got, err := r.DeleteInScope(context.Background(), "s1", writeScope())
			if err != nil {
				t.Fatalf("DeleteInScope: %v", err)
			}
			if got != tc.want {
				t.Errorf("deleted = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUpdateInScope_BindsTheOrganizationLast mirrors the statement's shape: the
// id is $1 and the organization array is the final placeholder.
func TestUpdateInScope_BindsTheOrganizationLast(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	mock.ExpectQuery(`UPDATE state_sources SET[\s\S]*organization_id`).
		WithArgs("s1", "renamed", nil, sqlmock.AnyArg(), sqlmock.AnyArg(), nil, writeScope().OrgIDs).
		WillReturnRows(sqlmock.NewRows(sourceCols).
			AddRow("s1", "renamed", "local", "", []byte(`{}`), []byte(`{}`), nil, "2026-06-10", "2026-06-11", testOrgID))

	got, err := r.UpdateInScope(context.Background(), &Source{ID: "s1", Name: "renamed"}, writeScope())
	if err != nil {
		t.Fatalf("UpdateInScope: %v", err)
	}
	if got == nil || got.Name != "renamed" {
		t.Fatalf("UpdateInScope = %+v, want the updated row", got)
	}
}

// TestUpdateInScope_NoRowIsNotAnError. A row in another organization reaches the
// handler exactly as a row that does not exist: (nil, nil).
func TestUpdateInScope_NoRowIsNotAnError(t *testing.T) {
	db, mock := newMock(t)
	r := NewSourceRepository(db)

	mock.ExpectQuery(`UPDATE state_sources SET[\s\S]*organization_id`).
		WillReturnRows(sqlmock.NewRows(sourceCols))

	got, err := r.UpdateInScope(context.Background(), &Source{ID: "s1", Name: "x"}, writeScope())
	if err != nil {
		t.Fatalf("UpdateInScope: %v", err)
	}
	if got != nil {
		t.Fatalf("UpdateInScope = %+v, want nil for a row the caller cannot reach", got)
	}
}
