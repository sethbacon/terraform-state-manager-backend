package repositories

import (
	"database/sql"
	"testing"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// ---------------------------------------------------------------------------
// ScheduleRepository — organization-scoped reads (#393 Phase 3)
//
// These cover the SHAPE of the statement. Whether the shape returns the right
// rows is a question about PostgreSQL and is answered against a real one in
// internal/tenancy/scoped_read_integration_test.go — a mock cannot tell you that
// `NULL = ANY(...)` is not true, and that is the property the whole partition
// rests on.
//
// MUTATION-VERIFIED. The table is in the commit message; each case here was run
// against a deliberately broken repository and observed to fail.
// ---------------------------------------------------------------------------

// THE FAIL-CLOSED CASE, asserted as "no statement reached the database" rather
// than as "no rows came back". Those are different claims: a reader that issued
// the query and happened to match nothing would satisfy the second while
// remaining one predicate edit away from returning the whole table. A caller
// whose tenancy could not be established has no business SELECTing from
// schedules at all.
func TestScheduleRepository_ScopedReadsOnAnEmptyScopeTouchNothing(t *testing.T) {
	db, mock := newMock(t)
	r := NewScheduleRepository(db)

	out, err := r.ListInScope(ctx, tenantscope.Scope{})
	if err != nil {
		t.Fatalf("ListInScope on an empty scope: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("ListInScope on an empty scope returned %d schedules; it must return none", len(out))
	}

	got, err := r.GetByIDInScope(ctx, "sc1", tenantscope.Scope{})
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

func TestScheduleRepository_ListInScope_FiltersByOrganization(t *testing.T) {
	db, mock := newMock(t)
	r := NewScheduleRepository(db)
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA, scopeOrgB}}

	mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{scopeOrgA, scopeOrgB}).
		WillReturnRows(scheduleRow())

	out, err := r.ListInScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	if len(out) != 1 || out[0].ID != "sc1" {
		t.Errorf("unexpected schedules: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").WillReturnError(errDB)
	if _, err := r.ListInScope(ctx, scope); err == nil {
		t.Error("ListInScope swallowed the query error")
	}
}

func TestScheduleRepository_GetByIDInScope_FiltersByOrganization(t *testing.T) {
	db, mock := newMock(t)
	r := NewScheduleRepository(db)
	scope := tenantscope.Scope{OrgIDs: []string{scopeOrgA}}

	// The organization array is $1 and the id is $2, so both readers share one
	// predicate string. If that order is ever reversed this expectation says so,
	// and a by-id read bound to the wrong parameter is somebody else's row.
	mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{scopeOrgA}, "sc1").
		WillReturnRows(scheduleRow())
	got, err := r.GetByIDInScope(ctx, "sc1", scope)
	if err != nil || got == nil || got.ID != "sc1" {
		t.Fatalf("GetByIDInScope: %v %+v", err, got)
	}

	// A schedule in another organization is reported EXACTLY as one that does
	// not exist. Anything else lets a caller enumerate ids and learn which of
	// them name real schedules elsewhere in the deployment.
	mock.ExpectQuery("FROM schedules WHERE organization_id = ANY").
		WithArgs([]string{scopeOrgA}, "elsewhere").
		WillReturnError(sql.ErrNoRows)
	got, err = r.GetByIDInScope(ctx, "elsewhere", scope)
	if err != nil || got != nil {
		t.Errorf("a schedule outside the scope must be (nil, nil), got %+v %v", got, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A platform admin reads unfiltered — TSM's only principal that is not somebody's
// tenant admin. Asserted as "the statement carried no organization argument",
// which is what distinguishes the unscoped query from the scoped one, and it
// also keeps them able to reach a row whose organization_id is NULL: a database
// restored from a backup taken before migration 000034 still holds those, and
// the organization predicate can never match one.
func TestScheduleRepository_ScopedReads_PlatformAdminIsUnfiltered(t *testing.T) {
	db, mock := newMock(t)
	r := NewScheduleRepository(db)
	admin := tenantscope.Scope{PlatformAdmin: true}

	mock.ExpectQuery("FROM schedules ORDER BY created_at DESC").
		WithArgs().
		WillReturnRows(scheduleRow())
	if _, err := r.ListInScope(ctx, admin); err != nil {
		t.Fatalf("ListInScope(platform admin): %v", err)
	}

	mock.ExpectQuery("FROM schedules WHERE id").
		WithArgs("sc1").
		WillReturnRows(scheduleRow())
	if _, err := r.GetByIDInScope(ctx, "sc1", admin); err != nil {
		t.Fatalf("GetByIDInScope(platform admin): %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestScheduleRepository_GetDueStaysUnscoped records a DECISION, not an
// oversight, and it is here so that nobody "fixes" it later.
//
// The background runner has no request and therefore no principal, so there is
// no scope to resolve; the #393 background-authority decision (option B) makes
// the split explicit: ENUMERATION STAYS UNSCOPED BY DESIGN -- reading every due
// schedule across organizations is the system's job -- while every per-item
// load after it runs under a scope DERIVED from the enumerated row
// (tenancy.SystemActingIn). GetDue reads every due schedule and carries each
// one's organization forward in memory across the dispatcher seam, because a
// schedule names its target only inside target_config JSONB and a run fired
// from it has no edge to join back along.
//
// Scoping GetDue to nothing — the reflex when a reader has no principal — would
// stop every cron schedule in the deployment from ever firing, silently.
func TestScheduleRepository_GetDueStaysUnscoped(t *testing.T) {
	db, mock := newMock(t)
	r := NewScheduleRepository(db)

	now := time.Now()
	mock.ExpectQuery("FROM schedules WHERE enabled AND next_run_at IS NOT NULL").
		WithArgs(now).
		WillReturnRows(scheduleRow())
	due, err := r.GetDue(ctx, now)
	if err != nil {
		t.Fatalf("GetDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("GetDue returned %d schedules, want 1. The background runner must still see "+
			"every due schedule: it has no principal to scope by, and a runner that reads "+
			"nothing stops every cron in the deployment without saying so.", len(due))
	}
	if due[0].OrganizationID != testOrgID {
		t.Errorf("a due schedule carries organization %q, want %q. The runner has no request to "+
			"derive one from, so this value is the only thing that says which tenant the run it "+
			"fires belongs to.", due[0].OrganizationID, testOrgID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
