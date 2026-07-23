package leaderelect

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func lockRows(got bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(got)
}

func unlockRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true)
}

// newElector wires an Elector over sqlmock with counters for the start/stop
// callbacks and a fast retry interval.
func newElector(t *testing.T, starts, stops *atomic.Int32) (*Elector, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	e := New(db, func() func() {
		starts.Add(1)
		return func() { stops.Add(1) }
	})
	e.interval = 10 * time.Millisecond
	return e, mock
}

func TestElector_WinsAndStopsCleanly(t *testing.T) {
	var starts, stops atomic.Int32
	e, mock := newElector(t, &starts, &stops)

	mock.ExpectQuery("pg_try_advisory_lock").WillReturnRows(lockRows(true))
	mock.MatchExpectationsInOrder(false)
	// Leader verifies its session on each tick until Stop.
	for range [64]struct{}{} {
		mock.ExpectPing()
	}
	mock.ExpectQuery("pg_advisory_unlock").WillReturnRows(unlockRows())

	e.Start()
	deadline := time.Now().Add(2 * time.Second)
	for starts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.Load() != 1 {
		t.Fatalf("start callbacks = %d, want 1", starts.Load())
	}
	e.Stop() // blocks until teardown
	if stops.Load() != 1 {
		t.Fatalf("stop callbacks = %d, want 1", stops.Load())
	}
}

func TestElector_StandbyPromotesOnRetry(t *testing.T) {
	var starts, stops atomic.Int32
	e, mock := newElector(t, &starts, &stops)

	// First attempt loses (another replica leads), second wins.
	mock.ExpectQuery("pg_try_advisory_lock").WillReturnRows(lockRows(false))
	mock.ExpectQuery("pg_try_advisory_lock").WillReturnRows(lockRows(true))
	mock.MatchExpectationsInOrder(false)
	for range [64]struct{}{} {
		mock.ExpectPing()
	}
	mock.ExpectQuery("pg_advisory_unlock").WillReturnRows(unlockRows())

	e.Start()
	deadline := time.Now().Add(2 * time.Second)
	for starts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.Load() != 1 {
		t.Fatalf("standby never promoted (starts=%d)", starts.Load())
	}
	e.Stop()
}

func TestElector_StepsDownWhenSessionDies(t *testing.T) {
	var starts, stops atomic.Int32
	e, mock := newElector(t, &starts, &stops)

	// Win, then the session dies on the first verify: workers must stop and
	// the loop re-campaigns (second win proves it kept going).
	mock.ExpectQuery("pg_try_advisory_lock").WillReturnRows(lockRows(true))
	mock.ExpectPing().WillReturnError(errors.New("session gone"))
	mock.ExpectQuery("pg_advisory_unlock").WillReturnError(errors.New("session gone"))
	mock.ExpectQuery("pg_try_advisory_lock").WillReturnRows(lockRows(true))
	mock.MatchExpectationsInOrder(false)
	for range [64]struct{}{} {
		mock.ExpectPing()
	}
	mock.ExpectQuery("pg_advisory_unlock").WillReturnRows(unlockRows())

	e.Start()
	deadline := time.Now().Add(2 * time.Second)
	for starts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.Load() < 2 {
		t.Fatalf("leader did not re-campaign after losing its session (starts=%d, stops=%d)", starts.Load(), stops.Load())
	}
	if stops.Load() < 1 {
		t.Fatalf("workers were not stopped on leadership loss (stops=%d)", stops.Load())
	}
	e.Stop()
}
