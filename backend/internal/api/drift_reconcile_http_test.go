package api

import (
	"net/http"
	"testing"
)

// Once the reconciler expires a run it clears the callback token. A late or
// replayed callback then loads a run whose token is empty and is rejected with
// the same uniform 401 as any invalid token — never a 200, so it cannot overwrite
// the failed outcome, and never an existence oracle. Critically, no consume/update
// runs (only the SELECT is expected), so the late callback has no effect.
func TestDriftLateCallbackAfterExpiryRejected(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(driftRow("")) // token cleared by the reconciler on expiry

	w := e.do(http.MethodPost, "/api/v1/drift/runs/d1/results",
		`{"token":"once-live-token","status":"completed","added":3,"changed":1}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("late callback to an expired run: status = %d, want 401 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an expired run must not be consumed or updated by a late callback: %v", err)
	}
}
