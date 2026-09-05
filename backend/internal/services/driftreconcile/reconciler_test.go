package driftreconcile

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/testsupport"
)

// captureLogs swaps in a buffering slog handler as the process default and
// restores the previous one on cleanup. Reconciler.logger is built from
// slog.With(...) in New, which resolves slog.Default() at construction time,
// so this must be called BEFORE constructing the Reconciler under test.
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// recordingNotifier captures the failure alerts the reconciler fires.
// testRunOrganization is the organization the seeded run belongs to.
const testRunOrganization = "11111111-1111-4111-8111-111111111111"

type recordingNotifier struct {
	mu    sync.Mutex
	calls []string // "runID|detail"
	// orgs records the organization each alert carried, so a test can assert the
	// fan-out is scoped rather than deployment-wide (#459).
	orgs []string
}

func (n *recordingNotifier) NotifyRunFailed(organizationID, runID, detail string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, runID+"|"+detail)
	n.orgs = append(n.orgs, organizationID)
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func dispatchedRow(id, token string) *sqlmock.Rows {
	return testsupport.DriftRunRow(id, "p1", nil, "app.tfstate", "", "", "dispatched", nil, nil, nil, nil, nil, "", token, "alice",
		"2026-06-21 10:00:00", "2026-06-21 10:00:00", false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
		nil, "", "")
}

// frozenNow is the reconciler's injected clock so the cutoff is deterministic and
// the test never waits real time.
var frozenNow = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

func newReconciler(t *testing.T, n FailureNotifier) (*Reconciler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	r := New(repositories.NewDriftRepository(db), n, 2*time.Hour, 5*time.Minute)
	r.now = func() time.Time { return frozenNow }
	return r, mock
}

// Happy expiry path: a run dispatched longer ago than the TTL is failed and a
// notification fires. The cutoff is asserted exactly (now - ttl) to prove the
// injected clock and TTL drive the sweep.
func TestReconciler_ExpiresStuckDispatchedRun(t *testing.T) {
	n := &recordingNotifier{}
	r, mock := newReconciler(t, n)

	cutoff := frozenNow.Add(-2 * time.Hour)
	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched' AND created_at <").
		WithArgs(cutoff, sqlmock.AnyArg()).
		WillReturnRows(dispatchedRow("d1", "live-token"))
	mock.ExpectExec("UPDATE drift_runs SET status='failed'").
		WithArgs("d1", "live-token", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	r.reconcileOnce(context.Background())

	if n.count() != 1 {
		t.Fatalf("expected 1 failure notification, got %d", n.count())
	}
	if !strings.HasPrefix(n.calls[0], "d1|") || !strings.Contains(n.calls[0], "expired: no callback") {
		t.Errorf("notification should carry the run id + expiry detail, got %q", n.calls[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the run must be expired via the repository: %v", err)
	}
	// The alert must carry the run's OWNING organization. Without it the fan-out
	// selects every enabled channel in the deployment, so one tenant's stuck run
	// is announced to every other tenant's webhooks (#459). The organization
	// travels on this seam because this is where the run object is in hand.
	if len(n.orgs) != 1 || n.orgs[0] != testRunOrganization {
		t.Errorf("alert carried organizations %v, want [%s]", n.orgs, testRunOrganization)
	}
}

// Not-yet-expired (no-op) path: nothing is older than the cutoff, so the sweep
// makes no writes and fires nothing.
func TestReconciler_NotYetExpiredIsNoop(t *testing.T) {
	n := &recordingNotifier{}
	r, mock := newReconciler(t, n)

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched' AND created_at <").
		WillReturnRows(sqlmock.NewRows(testsupport.DriftRunColumns)) // nothing past the cutoff

	r.reconcileOnce(context.Background())

	if n.count() != 0 {
		t.Errorf("no expired runs → no notifications, got %d", n.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("only the select should run: %v", err)
	}
}

// Race-guard: a real callback claimed the run between the select and the expire
// write (0 rows updated). The reconciler must not notify, so a run that actually
// completed/failed via callback never produces a spurious failure alert.
func TestReconciler_SkipsRunClaimedByCallback(t *testing.T) {
	n := &recordingNotifier{}
	r, mock := newReconciler(t, n)

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched' AND created_at <").
		WillReturnRows(dispatchedRow("d1", "live-token"))
	mock.ExpectExec("UPDATE drift_runs SET status='failed'").
		WithArgs("d1", "live-token", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0)) // callback won the race

	r.reconcileOnce(context.Background())

	if n.count() != 0 {
		t.Errorf("a run claimed by a callback must not notify, got %d", n.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// A query failure is non-fatal and fires nothing — the sweep retries next tick.
func TestReconciler_QueryFailureIsNonFatal(t *testing.T) {
	n := &recordingNotifier{}
	r, _ := newReconciler(t, n) // no expectations → the select errors
	r.reconcileOnce(context.Background())
	if n.count() != 0 {
		t.Errorf("nothing must fire when the select fails, got %d", n.count())
	}
}

// newReconcilerWithRecords is newReconciler plus a DriftRecordRepository
// attached over the SAME sqlmock database, so a test can expect queries
// against both drift_runs (via the Reconciler's own repo) and drift_records
// (via AttachDriftRecords) on one shared mock.
func newReconcilerWithRecords(t *testing.T, n FailureNotifier) (*Reconciler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	r := New(repositories.NewDriftRepository(db), n, 2*time.Hour, 5*time.Minute)
	r.now = func() time.Time { return frozenNow }
	r.AttachDriftRecords(repositories.NewDriftRecordRepository(db))
	return r, mock
}

// TestReconciler_RefreshesOpenRecordsMetric pins Phase 4a's leftover Phase 2
// metric: once records are attached, EVERY tick queries
// CountOpenBySeverity to refresh tsm_drift_records_open{severity},
// unconditionally (no retention config required).
func TestReconciler_RefreshesOpenRecordsMetric(t *testing.T) {
	n := &recordingNotifier{}
	r, mock := newReconcilerWithRecords(t, n)

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched'").
		WillReturnRows(sqlmock.NewRows(testsupport.DriftRunColumns))
	mock.ExpectQuery(`SELECT severity, COUNT\(\*\) FROM drift_records WHERE status='open' GROUP BY severity`).
		WillReturnRows(sqlmock.NewRows([]string{"severity", "count"}).AddRow("critical", 2).AddRow("warning", 5))

	r.reconcile()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the metric refresh must query the repository every tick: %v", err)
	}
}

// TestReconciler_NoExtraQueriesWithoutAttachment: a Reconciler built the old
// way (no AttachDriftRecords/EnableRetention call) behaves EXACTLY as before —
// no extra queries on tick, same as every existing reconciler_test
// expectation set already assumes.
func TestReconciler_NoExtraQueriesWithoutAttachment(t *testing.T) {
	n := &recordingNotifier{}
	r, mock := newReconciler(t, n)

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched'").
		WillReturnRows(sqlmock.NewRows(testsupport.DriftRunColumns))

	r.reconcile()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no records repo attached must mean no extra queries: %v", err)
	}
}

// TestReconciler_PrunesOnTickWhenRetentionEnabled pins the Phase 4a retention
// sweep: once EnableRetention is called, every tick also prunes drift_runs and
// resolved drift_records -- on the EXISTING reconciler tick, no new goroutine.
func TestReconciler_PrunesOnTickWhenRetentionEnabled(t *testing.T) {
	n := &recordingNotifier{}
	r, mock := newReconcilerWithRecords(t, n)
	r.EnableRetention(20, 90*24*time.Hour, 180*24*time.Hour)

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched'").
		WillReturnRows(sqlmock.NewRows(testsupport.DriftRunColumns))
	mock.ExpectQuery(`SELECT severity, COUNT\(\*\) FROM drift_records WHERE status='open' GROUP BY severity`).
		WillReturnRows(sqlmock.NewRows([]string{"severity", "count"}))
	mock.ExpectExec(`DELETE FROM drift_runs r`).
		WithArgs(20, (90 * 24 * time.Hour).Seconds()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`DELETE FROM drift_records WHERE status='resolved'`).
		WithArgs((180 * 24 * time.Hour).Seconds()).
		WillReturnResult(sqlmock.NewResult(0, 9))

	r.reconcile()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("retention must prune both tables via the repositories: %v", err)
	}
}

// TestReconciler_NoPruneWhenRetentionDisabled: records attached (for the open-
// records metric) but EnableRetention never called -- no prune queries.
//
// The retention window fields are poked directly to valid, non-zero values
// (this test is IN package driftreconcile, so it can) rather than left at
// their zero defaults. Left at zero, DriftRepository.PruneRuns and
// DriftRecordRepository.PruneResolved would refuse the call themselves before
// any statement reached the mock, which would make this test pass whether or
// not the reconciler's OWN retentionEnabled gate exists. Forcing valid values
// means the only thing standing between this tick and two DELETEs is that
// gate.
func TestReconciler_NoPruneWhenRetentionDisabled(t *testing.T) {
	logs := captureLogs(t)
	n := &recordingNotifier{}
	r, mock := newReconcilerWithRecords(t, n)
	r.retentionKeepPerState = 20
	r.retentionMaxAge = 90 * 24 * time.Hour
	r.retentionResolvedMaxAge = 180 * 24 * time.Hour

	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched'").
		WillReturnRows(sqlmock.NewRows(testsupport.DriftRunColumns))
	mock.ExpectQuery(`SELECT severity, COUNT\(\*\) FROM drift_records WHERE status='open' GROUP BY severity`).
		WillReturnRows(sqlmock.NewRows([]string{"severity", "count"}))

	r.reconcile()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no EnableRetention call must mean no prune queries even with a valid window configured: %v", err)
	}
	// The window fields above are deliberately non-zero, so PruneRuns/
	// PruneResolved's own input guards cannot be what is stopping this: an
	// attempted-but-unregistered DELETE would surface as a logged "failed to
	// prune" error (sqlmock rejects a query it was never told to expect), so
	// its absence is the actual proof the reconciler's retentionEnabled gate,
	// not the repository's argument validation, is what kept this tick clean.
	if strings.Contains(logs.String(), "failed to prune") {
		t.Errorf("a prune was attempted even though EnableRetention was never called: %s", logs.String())
	}
}

func TestReconciler_StartStop(t *testing.T) {
	n := &recordingNotifier{}
	r, mock := newReconciler(t, n)
	r.interval = 10 * time.Millisecond

	// Startup sweep + at least one tick; every sweep finds nothing to expire.
	for range [8]struct{}{} {
		mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched'").
			WillReturnRows(sqlmock.NewRows(testsupport.DriftRunColumns))
	}

	r.Start()
	time.Sleep(35 * time.Millisecond)
	r.Stop()
	time.Sleep(15 * time.Millisecond)

	if n.count() != 0 {
		t.Errorf("no expired runs → no notifications, got %d", n.count())
	}
}
