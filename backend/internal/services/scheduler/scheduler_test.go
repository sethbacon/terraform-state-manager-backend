package scheduler

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

func TestComputeNextRun_Cron(t *testing.T) {
	from := time.Date(2026, 6, 9, 10, 30, 0, 0, time.UTC)
	// Daily at 03:00 — next occurrence is the following day at 03:00.
	got := ComputeNextRun("0 3 * * *", from)
	if got == nil {
		t.Fatal("expected a next run for a valid cron expression")
	}
	want := time.Date(2026, 6, 10, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("next = %v, want %v", got, want)
	}
}

func TestComputeNextRun_Keywords(t *testing.T) {
	from := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"daily":     24 * time.Hour,
		"weekly":    7 * 24 * time.Hour,
		"every 15m": 15 * time.Minute,
		"every 2h":  2 * time.Hour,
		"every 10s": time.Minute, // sub-minute is clamped up to 1 minute
	}
	for expr, delta := range cases {
		got := ComputeNextRun(expr, from)
		if got == nil {
			t.Fatalf("%q: expected a next run", expr)
		}
		if want := from.Add(delta); !got.Equal(want) {
			t.Errorf("%q: next = %v, want %v", expr, got, want)
		}
	}
}

func TestComputeNextRun_Invalid(t *testing.T) {
	from := time.Now()
	for _, expr := range []string{"", "not-a-cron", "every", "every banana", "hourly"} {
		if got := ComputeNextRun(expr, from); got != nil {
			t.Errorf("ComputeNextRun(%q) = %v, want nil", expr, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase 2 — scheduler pacing (fleet-scale drift plan, §5 Phase 2)
//
// Reuses recordingDispatcher, dueRow/schedCols and newRunner from
// runner_test.go rather than a second set of fixtures for the same package.
// ---------------------------------------------------------------------------

func testSchedule(nextRunAt string) *repositories.Schedule {
	return &repositories.Schedule{
		ID: "sc1", Name: "nightly drift", CronExpr: "0 3 * * *", TargetType: "drift_run",
		Enabled: true, NextRunAt: &nextRunAt, OrganizationID: testScheduleOrg,
	}
}

// TestCheckDue_RespectsBatchLimit proves the configured BatchLimit reaches
// GetDue as its own argument (the per-tick bound on how many due schedules one
// poll reads), not a hardcoded or ignored value.
func TestCheckDue_RespectsBatchLimit(t *testing.T) {
	d := &recordingDispatcher{}
	r, mock := newRunner(t, d, Options{BatchLimit: 7})

	mock.ExpectQuery("FROM schedules WHERE enabled AND next_run_at IS NOT NULL").
		WithArgs(sqlmock.AnyArg(), 7).
		WillReturnRows(sqlmock.NewRows(schedCols))

	r.checkDue()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("checkDue must poll GetDue with the configured batch limit (7): %v", err)
	}
}

// TestFire_DefersWhenInFlightCapReached_NoClaim is the scheduler's at-most-once
// invariant under pacing: when the in-flight cap is reached, fire must return
// BEFORE calling ClaimDue, so the row's next_run_at is untouched and the same
// schedule is retried on a later poll rather than losing its turn.
//
// The registered ClaimDue expectation is deliberately left UNMATCHED. That is
// the load-bearing assertion, not "dispatch didn't run": a mutant that checks
// the cap AFTER claiming (or removes the check and merely fails to dispatch
// for some other reason) would still leave dispatch un-run while having
// already burned the claim -- which starves the schedule instead of retrying
// it. Only "ClaimDue was never invoked" rules that out, and sqlmock proves
// absence here: if ClaimDue's UPDATE ever reaches the database, it consumes
// this expectation and ExpectationsWereMet reports no error, which the
// assertion below treats as failure.
func TestFire_DefersWhenInFlightCapReached_NoClaim(t *testing.T) {
	d := &recordingDispatcher{runID: "run-1", status: "success"}
	r, mock := newRunner(t, d, Options{
		MaxInFlight: 1,
		InFlight:    func(ctx context.Context) (int, error) { return 1, nil }, // at the cap
	})

	mock.ExpectExec("UPDATE schedules").WillReturnResult(sqlmock.NewResult(0, 1))

	r.fire(testSchedule("2026-06-11 02:00:00"))

	if d.callCount() != 0 {
		t.Errorf("dispatch must not run while the schedule is deferred under the in-flight cap, got %d calls", d.callCount())
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("expected the ClaimDue UPDATE to remain unmatched -- fire must return before " +
			"claiming when the in-flight cap is reached. Getting no unmet expectations here means " +
			"ClaimDue was actually invoked, i.e. the cap check did not defer before the claim.")
	}
}

// TestFire_ProceedsUnderCap is the control for the test above: with the same
// shape but an in-flight count under the cap, ClaimDue and RecordOutcome must
// both reach the database and dispatch must run -- proving the cap check
// blocks ONLY the over-cap case, not firing in general.
func TestFire_ProceedsUnderCap(t *testing.T) {
	d := &recordingDispatcher{runID: "run-1", status: "success"}
	r, mock := newRunner(t, d, Options{
		MaxInFlight: 2,
		InFlight:    func(ctx context.Context) (int, error) { return 0, nil }, // under the cap
	})

	mock.ExpectExec("UPDATE schedules"). // ClaimDue
						WithArgs("sc1", "2026-06-11 02:00:00", sqlmock.AnyArg(), sqlmock.AnyArg()).
						WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE schedules SET last_status"). // RecordOutcome
								WithArgs("sc1", "success", "run-1").
								WillReturnResult(sqlmock.NewResult(0, 1))

	r.fire(testSchedule("2026-06-11 02:00:00"))

	if d.callCount() != 1 {
		t.Errorf("dispatch must run when the in-flight count is under the cap, got %d calls", d.callCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ClaimDue and RecordOutcome must both reach the database when under the cap: %v", err)
	}
}
