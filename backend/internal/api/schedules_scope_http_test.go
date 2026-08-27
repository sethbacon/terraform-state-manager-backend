package api

import (
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// The Phase 3 read flip for schedules, at the HTTP boundary — #393.
//
// The repository-level tests beside these (schedule_scope_test.go) pin the
// STATEMENT. These pin the three things only a handler can get wrong: refusing
// an unresolved scope instead of reading unscoped, returning nothing rather than
// everything on an empty one, and not dispatching a schedule the caller cannot
// see.
//
// MUTATION-VERIFIED — every case below was run against a deliberately broken
// tree and observed to fail. The table is in the commit message.

// TestScheduleReadRoutes_RefuseAnUnresolvedScope. If middleware.TenantScope came
// unwired on one of these routes, treating the absence as "no scope, carry on"
// would silently restore the unscoped read. It is a wiring fault, not an empty
// scope, and the two must not be conflated: tenantscope.FromContext returns
// (Scope{}, false) for the first and (Scope{}, true) for the second.
func TestScheduleReadRoutes_RefuseAnUnresolvedScope(t *testing.T) {
	for _, tc := range []struct{ name, method, path string }{
		{"list", http.MethodGet, "/api/v1/schedules"},
		{"get", http.MethodGet, "/api/v1/schedules/sc1"},
		{"run", http.MethodPost, "/api/v1/schedules/sc1/run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSchedulesEnvWithScope(t, nil)
			// No statements scripted: reaching the database at all is the failure.
			w := e.do(tc.method, tc.path, "")
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
			}
			if err := e.mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("a statement ran with no tenant scope resolved: %v", err)
			}
			if e.dispatcher.calls != 0 {
				t.Errorf("dispatcher was called %d time(s) with no tenant scope resolved", e.dispatcher.calls)
			}
		})
	}
}

// TestScheduleReadRoutes_OnAnEmptyScopeSeeNothing.
//
// An empty scope is an ANSWERED question — a caller who holds the required scope
// in no organization — and the answer is "nothing", never "everything". It is
// asserted here as "no statement reached the database", which is a stronger and
// different claim from "no rows came back": a reader that issued the query and
// happened to match nothing would satisfy the second while remaining one
// predicate edit away from returning the whole table.
func TestScheduleReadRoutes_OnAnEmptyScopeSeeNothing(t *testing.T) {
	empty := tenantscope.Scope{}

	e := newSchedulesEnvWithScope(t, &empty)
	w := e.do(http.MethodGet, "/api/v1/schedules", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d (%s)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); !strings.Contains(got, `"schedules":[]`) {
		t.Errorf("list on an empty scope returned %s; it must return no schedules", got)
	}

	if w := e.do(http.MethodGet, "/api/v1/schedules/sc1", ""); w.Code != http.StatusNotFound {
		t.Errorf("get on an empty scope: status = %d, want 404", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/schedules/sc1/run", ""); w.Code != http.StatusNotFound {
		t.Errorf("run on an empty scope: status = %d, want 404", w.Code)
	}
	if e.dispatcher.calls != 0 {
		t.Errorf("dispatcher was called %d time(s) on an empty scope", e.dispatcher.calls)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran for a caller whose scope reaches no organization: %v", err)
	}
}

// TestRunSchedule_RefusesAnotherOrganizationsSchedule is the case that makes this
// root's read flip an execution fix rather than a disclosure fix.
//
// RunSchedule loads a schedule by id and hands its target_config to the
// dispatcher stamped with the SCHEDULE's organization. Unscoped, a caller
// holding sources:manage in one organization could therefore fire another
// organization's schedule, against that organization's pipeline connection,
// decrypting its CI token to do it. The scoped read returns no row, so the
// dispatcher is never reached — which is what is asserted, because a 404 alone
// would also be produced by a handler that dispatched first and formatted its
// response afterwards.
func TestRunSchedule_RefusesAnotherOrganizationsSchedule(t *testing.T) {
	e := newSchedulesEnv(t)
	e.mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "sc1").
		WillReturnRows(sqlmock.NewRows(scheduleHTTPCols)) // owned elsewhere: no row

	if w := e.do(http.MethodPost, "/api/v1/schedules/sc1/run", ""); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if e.dispatcher.calls != 0 {
		t.Errorf("dispatcher was called %d time(s) for a schedule in another organization; "+
			"the run would have executed on that organization's pipeline connection",
			e.dispatcher.calls)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the by-id load was not scoped to the caller's organization: %v", err)
	}
}

// A platform administrator is the one principal that is genuinely
// deployment-wide, so neither reader may send them through the organization
// predicate. Asserted as "the statement bound no organization array", which is
// exactly what separates the unscoped query from the scoped one.
func TestScheduleReadRoutes_PlatformAdminIsUnfiltered(t *testing.T) {
	admin := tenantscope.Scope{PlatformAdmin: true}
	e := newSchedulesEnvWithScope(t, &admin)

	e.mock.ExpectQuery("FROM schedules ORDER BY created_at DESC").WithArgs().
		WillReturnRows(scheduleHTTPRow())
	if w := e.do(http.MethodGet, "/api/v1/schedules", ""); w.Code != http.StatusOK {
		t.Fatalf("list: status = %d (%s)", w.Code, w.Body.String())
	}

	e.mock.ExpectQuery("FROM schedules WHERE id").WithArgs("sc1").WillReturnRows(scheduleHTTPRow())
	if w := e.do(http.MethodGet, "/api/v1/schedules/sc1", ""); w.Code != http.StatusOK {
		t.Fatalf("get: status = %d (%s)", w.Code, w.Body.String())
	}

	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a platform admin was sent through the organization predicate: %v", err)
	}
}
