package api

import (
	"net/http"
	"testing"
)

// The version-lab runner always posts status="completed" and reports a broken
// version combination via success=false (it never posts status="failed"). So the
// real-callback alert must key on success, not the status string — otherwise a
// genuine init/plan failure would never notify. The reconciler, by contrast,
// sends an explicit failed status. healthResultFailed must alert on both.
func TestHealthResultFailed(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		success bool
		want    bool
	}{
		{"runner success: no alert", "completed", true, false},
		{"runner init/plan failure (status still completed)", "completed", false, true},
		{"reconciler expiry (explicit failed status)", "failed", false, true},
		{"explicit failed status with success flag", "failed", true, true},
	}
	for _, tc := range cases {
		if got := healthResultFailed(tc.status, tc.success); got != tc.want {
			t.Errorf("%s: healthResultFailed(%q, %v) = %v, want %v", tc.name, tc.status, tc.success, got, tc.want)
		}
	}
}

// Once the background reconciler expires a stuck health run it clears the stored
// callback_token. A late callback from the CI job that finally came back must then
// be rejected with the uniform 401 — the cleared token closes the late-callback
// hole, exactly as the drift path does. The handler must short-circuit on the
// empty stored token (mirroring drift.go:411-415): no ConsumeCallbackToken /
// UpdateResult is expected, so the only DB op is the GetByID select.
func TestHealthRun_ClearedTokenCallbackRejected(t *testing.T) {
	e := newDriftEnv(t)

	// Post-expiry row: same run id, but callback_token is now "".
	e.mock.ExpectQuery("SELECT .+ FROM health_runs WHERE id").WithArgs("h1").
		WillReturnRows(healthRow(""))

	// Even the CI job's original (now stale) token is rejected once the stored
	// token is blank — the empty-stored-token guard fires before any compare.
	w := e.doWithHeader(http.MethodPost, "/api/v1/health-lab/runs/h1/results",
		`{"status":"completed","init_ok":true,"plan_ok":true}`,
		"X-TSM-Callback-Token", "stale-but-real-token")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("late callback after expiry: status = %d, want 401 (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a cleared-token run must not be consumed/updated, only selected: %v", err)
	}

	// An empty token against the blank stored token must not slip through either
	// (empty == empty must still be a 401, not an authenticated match).
	e.mock.ExpectQuery("SELECT .+ FROM health_runs WHERE id").WithArgs("h1").
		WillReturnRows(healthRow(""))
	w = e.do(http.MethodPost, "/api/v1/health-lab/runs/h1/results", `{"token":""}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty token vs cleared token: status = %d, want 401", w.Code)
	}
}
