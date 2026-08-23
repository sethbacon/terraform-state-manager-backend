package healthreconcile

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

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

var healthCols = []string{"id", "pipeline_connection_id", "repo_ref", "working_dir", "terraform_version",
	"provider_versions", "module_versions", "registry_host", "status", "init_ok", "plan_ok", "success",
	"summary", "detail", "callback_token", "actor", "created_at", "updated_at", "organization_id"}

func dispatchedRow(id, token string) *sqlmock.Rows {
	return sqlmock.NewRows(healthCols).
		AddRow(id, "p1", "", "", "", []byte(`{}`), []byte(`{}`), "", "dispatched", nil, nil, nil,
			nil, "", token, "alice", "2026-06-21 10:00:00", "2026-06-21 10:00:00", "11111111-1111-4111-8111-111111111111")
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
	r := New(repositories.NewHealthRepository(db), n, 2*time.Hour, 5*time.Minute)
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
	mock.ExpectQuery("SELECT .+ FROM health_runs WHERE status='dispatched' AND created_at <").
		WithArgs(cutoff, sqlmock.AnyArg()).
		WillReturnRows(dispatchedRow("h1", "live-token"))
	mock.ExpectExec("UPDATE health_runs SET status='failed'").
		WithArgs("h1", "live-token", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	r.reconcileOnce(context.Background())

	if n.count() != 1 {
		t.Fatalf("expected 1 failure notification, got %d", n.count())
	}
	if !strings.HasPrefix(n.calls[0], "h1|") || !strings.Contains(n.calls[0], "expired: no callback") {
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

	mock.ExpectQuery("SELECT .+ FROM health_runs WHERE status='dispatched' AND created_at <").
		WillReturnRows(sqlmock.NewRows(healthCols)) // nothing past the cutoff

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

	mock.ExpectQuery("SELECT .+ FROM health_runs WHERE status='dispatched' AND created_at <").
		WillReturnRows(dispatchedRow("h1", "live-token"))
	mock.ExpectExec("UPDATE health_runs SET status='failed'").
		WithArgs("h1", "live-token", sqlmock.AnyArg()).
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

func TestReconciler_StartStop(t *testing.T) {
	n := &recordingNotifier{}
	r, mock := newReconciler(t, n)
	r.interval = 10 * time.Millisecond

	// Startup sweep + at least one tick; every sweep finds nothing to expire.
	for range [8]struct{}{} {
		mock.ExpectQuery("SELECT .+ FROM health_runs WHERE status='dispatched'").
			WillReturnRows(sqlmock.NewRows(healthCols))
	}

	r.Start()
	time.Sleep(35 * time.Millisecond)
	r.Stop()
	time.Sleep(15 * time.Millisecond)

	if n.count() != 0 {
		t.Errorf("no expired runs → no notifications, got %d", n.count())
	}
}
