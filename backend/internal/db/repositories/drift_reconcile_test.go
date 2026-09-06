package repositories

import (
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/terraform-state-manager/terraform-state-manager/internal/testsupport"
)

// dispatchedDriftRow is a drift_runs row still sitting in "dispatched" with a
// live callback token — what the reconciler selects and then expires. Reuses the
// shared testsupport.DriftRunColumns column set.
func dispatchedDriftRow(id, token string) *sqlmock.Rows {
	return testsupport.DriftRunRow(id, "p1", nil, "app.tfstate", "", "", "dispatched", nil, nil, nil, nil, nil, "", token, "alice",
		"2026-06-21 10:00:00", "2026-06-21 10:00:00", false, 0, 0, false, false, "11111111-1111-4111-8111-111111111111",
		nil, "", "", 0, 0, 0, nil)
}

func TestDriftRepository_ListExpiredDispatched(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	cutoff := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched' AND created_at <").
		WithArgs(cutoff, 100).
		WillReturnRows(dispatchedDriftRow("d1", "live-token"))

	out, err := r.ListExpiredDispatched(ctx, cutoff, 100)
	if err != nil || len(out) != 1 {
		t.Fatalf("ListExpiredDispatched: %v len=%d", err, len(out))
	}
	// Unlike List, the callback token is retained — the reconciler needs it to
	// expire the run race-safely (it only acts if the token is still the live one).
	if out[0].CallbackToken != "live-token" {
		t.Errorf("callback token must be retained for internal use, got %q", out[0].CallbackToken)
	}

	// A query error surfaces.
	mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE status='dispatched'").WillReturnError(errDB)
	if _, err := r.ListExpiredDispatched(ctx, cutoff, 100); err == nil {
		t.Error("ListExpiredDispatched swallowed the query error")
	}
}

func TestDriftRepository_ExpireDispatched(t *testing.T) {
	db, mock := newMock(t)
	r := NewDriftRepository(db)

	const detail = "expired: no callback within 2h0m0s"

	// We win the race: the run is still dispatched and still holds our token, so
	// the conditional UPDATE flips it to failed and clears the token in one shot.
	mock.ExpectExec("UPDATE drift_runs SET status='failed'").
		WithArgs("d1", "live-token", detail).
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := r.ExpireDispatched(ctx, "d1", "live-token", detail)
	if err != nil || !ok {
		t.Fatalf("ExpireDispatched should report it expired the run: ok=%v err=%v", ok, err)
	}

	// A concurrent callback consumed the token first (status moved / token cleared)
	// → 0 rows match → we did NOT expire it, so the caller must not double-fire.
	mock.ExpectExec("UPDATE drift_runs SET status='failed'").
		WithArgs("d1", "live-token", detail).
		WillReturnResult(sqlmock.NewResult(0, 0))
	ok, err = r.ExpireDispatched(ctx, "d1", "live-token", detail)
	if err != nil || ok {
		t.Errorf("a run already claimed by a callback must not be expired: ok=%v err=%v", ok, err)
	}

	// An empty token never runs a query (defensive — there is nothing to match).
	if ok, _ := r.ExpireDispatched(ctx, "d1", "", detail); ok {
		t.Error("empty token must be rejected without a query")
	}
}
