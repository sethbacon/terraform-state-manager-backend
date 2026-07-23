package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

type recordingDispatcher struct {
	mu     sync.Mutex
	calls  []string
	runID  string
	status string
	err    error
}

func (d *recordingDispatcher) Dispatch(_ context.Context, targetType string, _ json.RawMessage, actor string) (string, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, targetType+"/"+actor)
	return d.runID, d.status, d.err
}

func (d *recordingDispatcher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

var schedCols = []string{"id", "name", "cron_expr", "target_type", "target_config", "enabled",
	"last_run_at", "next_run_at", "last_run_id", "last_status", "created_at", "updated_at"}

func dueRow() *sqlmock.Rows {
	return sqlmock.NewRows(schedCols).
		AddRow("sc1", "nightly", "daily", "drift", []byte(`{"pipeline_connection_id":"p1"}`), true,
			nil, "2026-06-10 00:00:00", nil, nil, "2026-06-09", "2026-06-09")
}

func newRunner(t *testing.T, d Dispatcher) (*Runner, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(repositories.NewScheduleRepository(db), d), mock
}

func TestRunner_FiresDueScheduleAndReschedules(t *testing.T) {
	d := &recordingDispatcher{runID: "run-1", status: "success"}
	r, mock := newRunner(t, d)

	mock.ExpectQuery("FROM schedules WHERE enabled").WillReturnRows(dueRow())
	// The claim advances next_run_at (rescheduled from now — an overdue
	// schedule fires once, no catch-up storm) BEFORE the dispatch...
	mock.ExpectExec("UPDATE schedules").
		WithArgs("sc1", "2026-06-10 00:00:00", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// ...and the outcome write stamps status + run id only.
	mock.ExpectExec("UPDATE schedules SET last_status").
		WithArgs("sc1", "success", "run-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	r.checkDue()

	if d.callCount() != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", d.callCount())
	}
	if d.calls[0] != "drift/scheduler" {
		t.Errorf("dispatch actor/type = %s, want drift/scheduler", d.calls[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("claim then outcome must both be recorded: %v", err)
	}
}

// TestRunner_LostClaimSkipsDispatch: when the conditional claim matches no row
// (another poll or replica advanced next_run_at first, or the schedule was
// edited/disabled), the dispatcher must NOT run — that would be the duplicate
// CI dispatch this flow exists to prevent.
func TestRunner_LostClaimSkipsDispatch(t *testing.T) {
	d := &recordingDispatcher{runID: "run-1", status: "success"}
	r, mock := newRunner(t, d)

	mock.ExpectQuery("FROM schedules WHERE enabled").WillReturnRows(dueRow())
	mock.ExpectExec("UPDATE schedules").
		WithArgs("sc1", "2026-06-10 00:00:00", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0)) // zero rows: claim lost

	r.checkDue()

	if d.callCount() != 0 {
		t.Fatalf("lost claim must not dispatch, got %d calls", d.callCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRunner_ClaimErrorSkipsDispatch: a claim that errors (DB down) must not
// dispatch either — without the claim recorded, a dispatch would re-fire later.
func TestRunner_ClaimErrorSkipsDispatch(t *testing.T) {
	d := &recordingDispatcher{}
	r, mock := newRunner(t, d)

	mock.ExpectQuery("FROM schedules WHERE enabled").WillReturnRows(dueRow())
	mock.ExpectExec("UPDATE schedules").WillReturnError(errors.New("db down"))

	r.checkDue()
	if d.callCount() != 0 {
		t.Fatalf("claim error must not dispatch, got %d calls", d.callCount())
	}
}

func TestRunner_RecordsDispatchFailure(t *testing.T) {
	d := &recordingDispatcher{status: "", err: errors.New("provider down")}
	r, mock := newRunner(t, d)

	mock.ExpectQuery("FROM schedules WHERE enabled").WillReturnRows(dueRow())
	mock.ExpectExec("UPDATE schedules").
		WithArgs("sc1", "2026-06-10 00:00:00", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE schedules SET last_status").
		WithArgs("sc1", "failed", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	r.checkDue()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("failed dispatch must be recorded as failed with no run id: %v", err)
	}
}

func TestRunner_QueryFailureIsNonFatal(t *testing.T) {
	d := &recordingDispatcher{}
	r, _ := newRunner(t, d) // no expectations → GetDue errors
	r.checkDue()
	if d.callCount() != 0 {
		t.Error("nothing must fire when the due query fails")
	}
}

func TestRunner_StartStop(t *testing.T) {
	d := &recordingDispatcher{runID: "run-1", status: "success"}
	r, mock := newRunner(t, d)
	r.interval = 10 * time.Millisecond

	// Startup check + at least one tick; every poll returns no due rows.
	for range [8]struct{}{} {
		mock.ExpectQuery("FROM schedules WHERE enabled").WillReturnRows(sqlmock.NewRows(schedCols))
	}

	r.Start()
	time.Sleep(35 * time.Millisecond)
	r.Stop()
	time.Sleep(15 * time.Millisecond) // loop drains after Stop

	if d.callCount() != 0 {
		t.Errorf("no due schedules → no dispatches, got %d", d.callCount())
	}
}
