package repositories

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// The three outcomes, for each of the four remaining configuration roots.
//
// Table-driven over the roots rather than written out per repository, because
// the defect this closes was one rule applied to some of them and not the
// others. A per-root test file is how that happens again: the next root gets a
// file that does not exist yet, and nothing says so. Here, adding a root without
// adding a case is a visible gap in one table.

func rootScope() tenantscope.Scope {
	return tenantscope.Scope{OrgIDs: []string{"aaaaaaaa-0000-4000-8000-000000000001"}}
}

func TestScopedDeletes_AcrossTheConfigRoots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table string
		call  func(ctx context.Context, db *sql.DB, id string, scope tenantscope.Scope) (bool, error)
	}{
		{"schedules", "DELETE FROM schedules", func(ctx context.Context, d *sql.DB, id string, s tenantscope.Scope) (bool, error) {
			return NewScheduleRepository(d).DeleteInScope(ctx, id, s)
		}},
		{"pipeline_connections", "DELETE FROM pipeline_connections", func(ctx context.Context, d *sql.DB, id string, s tenantscope.Scope) (bool, error) {
			return NewPipelineRepository(d).DeleteInScope(ctx, id, s)
		}},
		{"ci_sources", "DELETE FROM ci_sources", func(ctx context.Context, d *sql.DB, id string, s tenantscope.Scope) (bool, error) {
			return NewCISourceRepository(d).DeleteInScope(ctx, id, s)
		}},
	} {
		t.Run(tc.name+"/empty scope runs nothing", func(t *testing.T) {
			db, mock := newMock(t)
			if _, err := tc.call(context.Background(), db, "x1", tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
				t.Fatalf("error = %v, want ErrNotInScope", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("an empty scope reached the database: %v", err)
			}
		})

		t.Run(tc.name+"/platform admin runs the unscoped statement", func(t *testing.T) {
			db, mock := newMock(t)
			// One argument: the unscoped statement binds only the id.
			mock.ExpectExec(tc.table).WithArgs("x1").WillReturnResult(sqlmock.NewResult(0, 1))
			if ok, err := tc.call(context.Background(), db, "x1", tenantscope.Scope{PlatformAdmin: true}); err != nil || !ok {
				t.Fatalf("= %v, %v; want true, nil", ok, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("a platform admin did not take the unscoped statement: %v", err)
			}
		})

		t.Run(tc.name+"/reports whether a row matched", func(t *testing.T) {
			for _, affected := range []int64{1, 0} {
				db, mock := newMock(t)
				mock.ExpectExec(tc.table).WithArgs("x1", rootScope().OrgIDs).
					WillReturnResult(sqlmock.NewResult(0, affected))
				got, err := tc.call(context.Background(), db, "x1", rootScope())
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if want := affected > 0; got != want {
					t.Errorf("affected=%d: deleted = %v, want %v", affected, got, want)
				}
			}
		})
	}
}

// TestScopedUpdates_RefuseAnEmptyScope covers the two updates, whose bodies
// differ too much to share a table with the deletes.
func TestScopedUpdates_RefuseAnEmptyScope(t *testing.T) {
	db, mock := newMock(t)
	if _, err := NewScheduleRepository(db).UpdateInScope(
		context.Background(), "s1", &Schedule{Name: "x"}, nil, tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
		t.Errorf("schedule update: error = %v, want ErrNotInScope", err)
	}
	if _, err := NewPipelineRepository(db).UpdateInScope(
		context.Background(), &PipelineConnection{ID: "p1"}, false, tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
		t.Errorf("pipeline update: error = %v, want ErrNotInScope", err)
	}
	if _, err := NewCISourceRepository(db).UpdateInScope(
		context.Background(), &CISource{ID: "c1"}, tenantscope.Scope{}); !errors.Is(err, ErrNotInScope) {
		t.Errorf("ci_source update: error = %v, want ErrNotInScope", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an empty scope reached the database: %v", err)
	}
}

// TestScopedUpdates_BindTheOrganizationLast pins the placeholder position, which
// is the thing a hand-written statement gets wrong.
func TestScopedUpdates_BindTheOrganizationLast(t *testing.T) {
	db, mock := newMock(t)
	mock.ExpectQuery(`UPDATE schedules[\s\S]*WHERE id=\$1 AND organization_id`).
		WithArgs("s1", "nightly", "0 3 * * *", "drift", sqlmock.AnyArg(), true, sqlmock.AnyArg(), rootScope().OrgIDs).
		WillReturnRows(sqlmock.NewRows(scheduleCols))
	if _, err := NewScheduleRepository(db).UpdateInScope(context.Background(), "s1",
		&Schedule{Name: "nightly", CronExpr: "0 3 * * *", TargetType: "drift", Enabled: true},
		ptrTime(time.Now()), rootScope()); err != nil {
		t.Fatalf("UpdateInScope: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the organization was not bound last: %v", err)
	}

	clientID := "wi-client"
	mock.ExpectQuery(`UPDATE ci_sources[\s\S]*WHERE id = \$1 AND organization_id`).
		WithArgs("c1", "corp-ado", "corp", (*string)(nil), "workload_identity",
			[]byte(nil), (*string)(nil), &clientID, []byte(nil), (*string)(nil), (*string)(nil), []byte(nil),
			rootScope().OrgIDs).
		WillReturnRows(sqlmock.NewRows(ciCols))
	if _, err := NewCISourceRepository(db).UpdateInScope(context.Background(),
		&CISource{ID: "c1", Name: "corp-ado", Organization: "corp", AuthMethod: "workload_identity", ClientID: &clientID},
		rootScope()); err != nil {
		t.Fatalf("CI source UpdateInScope: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the ci_source organization was not bound last: %v", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
