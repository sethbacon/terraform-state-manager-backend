package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// The analysis dual-read observation (#455) must not change what is served, and
// must not run unless the flag is on.
//
// These are properties of the HANDLER. The scoped reader's own correctness is
// covered against a real PostgreSQL in internal/db/repositories, because the
// tenant predicate there is a join and a mock cannot evaluate one.

// errObserve stands in for any failure of the observation query.
var errObserve = errors.New("scoped analysis read failed")

// dashboardRig builds the overview route with a scope stored, and scripts every
// read the handler makes except the observation's.
func dashboardRig(t *testing.T, dualRead bool) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)

	h := NewSourcesHandlers(db, nil)
	h.EnableTenantDualRead(dualRead)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		tenantscope.Store(c, tenantscope.Scope{OrgIDs: []string{testActingOrg}})
		c.Next()
	})
	r.GET("/api/v1/dashboard/overview", h.DashboardOverview())

	// The reads DashboardOverview makes, all unscoped today.
	mock.ExpectQuery("FROM state_sources").WillReturnRows(sqlmock.NewRows(apiSourceCols))
	mock.ExpectQuery("FROM source_sync_status").
		WillReturnRows(sqlmock.NewRows([]string{"source_id", "last_sync_at", "states_listed", "read_errors", "last_error", "states_stored"}))
	mock.ExpectQuery("FROM state_analyses").
		WillReturnRows(sqlmock.NewRows([]string{"count", "rum", "managed", "data", "total"}).AddRow(7, 70, 7, 0, 7))
	for i := 0; i < 3; i++ { // ProviderCounts, ResourceTypeCounts, VersionCounts
		mock.ExpectQuery("FROM state_analyses").WillReturnRows(sqlmock.NewRows([]string{"key", "n"}))
	}
	return r, mock
}

func getOverview(r *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/overview", nil))
	return w
}

// TestDashboardObservationIsOffByDefault. Every extra query on a dashboard load
// is paid by every operator, and the tenancy config is explicit that the flag
// exists because the measurement costs extra reads. Nothing scripts the scoped
// totals query, so if the observation runs it fails on an unexpected call.
func TestDashboardObservationIsOffByDefault(t *testing.T) {
	r, mock := dashboardRig(t, false)
	// SCRIPTED SO IT CAN GO UNCONSUMED. An earlier version simply left the query
	// unscripted and asserted ExpectationsWereMet — which passes either way,
	// because sqlmock reports UNFULFILLED expectations and not extra calls, and
	// the handler swallows the resulting error by design. Scripting it inverts
	// the test: the expectation must remain unmet, and that is detectable.
	mock.ExpectQuery("FROM state_analyses a").
		WillReturnRows(sqlmock.NewRows([]string{"count", "rum", "managed", "data", "total"}).AddRow(0, 0, 0, 0, 0))

	if w := getOverview(r); w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("the scoped observation query ran with the flag off; it costs an extra " +
			"read on every dashboard load and the flag exists to make that opt-in")
	}
}

// TestDashboardObservationDoesNotChangeTheResponse. It reports; it never
// withholds. A divergence is the leak being observed rather than a fault, and
// failing the request on one would make the flag itself the partial cutover
// migration 000033 refuses.
func TestDashboardObservationDoesNotChangeTheResponse(t *testing.T) {
	off, _ := dashboardRig(t, false)
	before := getOverview(off).Body.String()

	on, mock := dashboardRig(t, true)
	// The observation's scoped read, returning a SMALLER count than the 7 the
	// unscoped read served — a real divergence, which must still not reach the
	// client.
	mock.ExpectQuery("FROM state_analyses a").
		WillReturnRows(sqlmock.NewRows([]string{"count", "rum", "managed", "data", "total"}).AddRow(2, 20, 2, 0, 2))

	after := getOverview(on).Body.String()
	if before != after {
		t.Errorf("the observation changed the served response.\n off: %s\n on:  %s", before, after)
	}
	// ...and the SCOPED query is the one it ran. The expectation above matches
	// `FROM state_analyses a` — the aliased, joined form — which the unscoped
	// Totals does not emit. Without this the observation could compare the
	// unscoped read against itself, report zero divergence forever, and look
	// exactly like a partition with nothing to report.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the observation did not run the scoped read: %v", err)
	}
}

// TestDashboardObservationSurvivesAFailedScopedRead. The measurement is
// best-effort: a scoped read that errors must be logged and dropped, never
// turned into a 500 on a response the client has already been given.
func TestDashboardObservationSurvivesAFailedScopedRead(t *testing.T) {
	r, mock := dashboardRig(t, true)
	mock.ExpectQuery("FROM state_analyses a").WillReturnError(errObserve)

	w := getOverview(r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: a failed measurement broke a served read (%s)", w.Code, w.Body.String())
	}
	// The status alone proves nothing: the response is already written when the
	// observation runs, so a handler that turns the failure into an error writes
	// a SECOND body after a 200 and the recorder still reads 200. Two
	// concatenated JSON objects do not unmarshal, and that is what detects it.
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("a failed measurement appended a second response: %s", w.Body.String())
	}
}
