package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// fakeDispatcher records the dispatch and returns a scripted outcome.
type fakeDispatcher struct {
	runID  string
	status string
	err    error
	calls  int
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ string, _ json.RawMessage, _ string) (string, string, error) {
	f.calls++
	return f.runID, f.status, f.err
}

type schedulesEnv struct {
	*sourcesEnv
	dispatcher *fakeDispatcher
}

func newSchedulesEnv(t *testing.T) *schedulesEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	d := &fakeDispatcher{runID: "run-1", status: "success"}
	h := NewScheduleHandlers(db, nil, d)

	r := gin.New()
	v1 := r.Group("/api/v1")
	v1.GET("/schedules", h.ListSchedules())
	v1.POST("/schedules", h.CreateSchedule())
	v1.GET("/schedules/:id", h.GetSchedule())
	v1.PUT("/schedules/:id", h.UpdateSchedule())
	v1.DELETE("/schedules/:id", h.DeleteSchedule())
	v1.POST("/schedules/:id/run", h.RunSchedule())

	return &schedulesEnv{sourcesEnv: &sourcesEnv{r: r, mock: mock}, dispatcher: d}
}

var scheduleHTTPCols = []string{"id", "name", "cron_expr", "target_type", "target_config", "enabled",
	"last_run_at", "next_run_at", "last_run_id", "last_status", "created_at", "updated_at"}

func scheduleHTTPRow() *sqlmock.Rows {
	return sqlmock.NewRows(scheduleHTTPCols).
		AddRow("sc1", "nightly", "0 2 * * *", "drift", []byte(`{"pipeline_connection_id":"p1"}`), true,
			nil, "2026-06-11 02:00:00", nil, nil, "2026-06-10", "2026-06-10")
}

func TestSchedules_CRUD(t *testing.T) {
	e := newSchedulesEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM schedules ORDER BY").WillReturnRows(scheduleHTTPRow())
	if w := e.do(http.MethodGet, "/api/v1/schedules", ""); w.Code != http.StatusOK {
		t.Errorf("list: status = %d", w.Code)
	}

	// Validation: bad cron, unsupported target type, missing pipeline id.
	if w := e.do(http.MethodPost, "/api/v1/schedules", `{"name":"x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing cron: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/schedules",
		`{"name":"x","cron_expr":"not a cron","target_config":{"pipeline_connection_id":"p1"}}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad cron: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/schedules",
		`{"name":"x","cron_expr":"daily","target_type":"webhook"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unsupported target: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/schedules",
		`{"name":"x","cron_expr":"daily","target_config":{}}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing pipeline id: status = %d, want 400", w.Code)
	}

	e.mock.ExpectQuery("INSERT INTO schedules").WillReturnRows(scheduleHTTPRow())
	w := e.do(http.MethodPost, "/api/v1/schedules",
		`{"name":"nightly","cron_expr":"0 2 * * *","target_config":{"pipeline_connection_id":"p1"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}

	e.mock.ExpectQuery("SELECT .+ FROM schedules WHERE id").WithArgs("sc1").WillReturnRows(scheduleHTTPRow())
	if w := e.do(http.MethodGet, "/api/v1/schedules/sc1", ""); w.Code != http.StatusOK {
		t.Errorf("get: status = %d", w.Code)
	}

	e.mock.ExpectQuery("SELECT .+ FROM schedules WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(scheduleHTTPCols))
	if w := e.do(http.MethodGet, "/api/v1/schedules/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing: status = %d, want 404", w.Code)
	}

	e.mock.ExpectQuery("UPDATE schedules").WillReturnRows(scheduleHTTPRow())
	w = e.do(http.MethodPut, "/api/v1/schedules/sc1",
		`{"name":"nightly","cron_expr":"daily","target_config":{"pipeline_connection_id":"p1"},"enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: status = %d (%s)", w.Code, w.Body.String())
	}

	e.mock.ExpectQuery("UPDATE schedules").WillReturnError(sql.ErrNoRows)
	if w := e.do(http.MethodPut, "/api/v1/schedules/ghost",
		`{"name":"x","cron_expr":"daily","target_config":{"pipeline_connection_id":"p1"}}`); w.Code != http.StatusNotFound {
		t.Errorf("update missing: status = %d, want 404", w.Code)
	}

	e.mock.ExpectExec("DELETE FROM schedules").WithArgs("sc1").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/schedules/sc1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w.Code)
	}
}

func TestSchedules_RunNow(t *testing.T) {
	e := newSchedulesEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM schedules WHERE id").WithArgs("sc1").WillReturnRows(scheduleHTTPRow())
	e.mock.ExpectExec("UPDATE schedules").WillReturnResult(sqlmock.NewResult(0, 1)) // RecordRun
	e.mock.ExpectQuery("SELECT .+ FROM schedules WHERE id").WithArgs("sc1").WillReturnRows(scheduleHTTPRow())

	if w := e.do(http.MethodPost, "/api/v1/schedules/sc1/run", ""); w.Code != http.StatusOK {
		t.Fatalf("run: status = %d (%s)", w.Code, w.Body.String())
	}
	if e.dispatcher.calls != 1 {
		t.Errorf("dispatcher calls = %d, want 1", e.dispatcher.calls)
	}

	// Dispatch failure surfaces as 502 but still records the outcome.
	e.dispatcher.err = errors.New("pipeline rejected the run")
	e.dispatcher.status = "failed"
	e.mock.ExpectQuery("SELECT .+ FROM schedules WHERE id").WithArgs("sc1").WillReturnRows(scheduleHTTPRow())
	e.mock.ExpectExec("UPDATE schedules").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodPost, "/api/v1/schedules/sc1/run", ""); w.Code != http.StatusBadGateway {
		t.Errorf("failed dispatch: status = %d, want 502", w.Code)
	}

	e.mock.ExpectQuery("SELECT .+ FROM schedules WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(scheduleHTTPCols))
	if w := e.do(http.MethodPost, "/api/v1/schedules/ghost/run", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing: status = %d, want 404", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Dashboard overview over a real local source
// ---------------------------------------------------------------------------

func TestDashboardOverview_Compute(t *testing.T) {
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	dir := t.TempDir()

	h := NewSourcesHandlers(db, nil)
	r := gin.New()
	r.GET("/api/v1/dashboard/overview", h.DashboardOverview())

	env := &sourcesEnv{r: r, mock: mock, dir: dir}
	env.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web", "aws_vpc.main"))

	cfg, _ := json.Marshal(map[string]any{"base_path": dir})
	sourceListRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
			AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10")
	}

	// Cold: computes across the (real) local source.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceListRows())
	w := env.do(http.MethodGet, "/api/v1/dashboard/overview", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cold overview: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"rum":2`) || !strings.Contains(w.Body.String(), "refreshed_at") {
		t.Errorf("overview aggregation wrong: %s", w.Body.String())
	}

	// Warm: served from cache — no DB expectation queued, must still 200.
	if w := env.do(http.MethodGet, "/api/v1/dashboard/overview", ""); w.Code != http.StatusOK {
		t.Errorf("warm overview: status = %d (cache miss?)", w.Code)
	}

	// refresh=true bypasses the cache and recomputes.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceListRows())
	if w := env.do(http.MethodGet, "/api/v1/dashboard/overview?refresh=true", ""); w.Code != http.StatusOK {
		t.Errorf("forced refresh: status = %d", w.Code)
	}

	// Refresher start/stop is clean.
	stop := h.StartOverviewRefresher()
	stop()
}

func TestDashboardOverview_SourceListError(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	h := NewSourcesHandlers(db, nil)
	r := gin.New()
	r.GET("/api/v1/dashboard/overview", h.DashboardOverview())

	env := &sourcesEnv{r: r}
	if w := env.do(http.MethodGet, "/api/v1/dashboard/overview", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on a cold cache + dead DB", w.Code)
	}
}
