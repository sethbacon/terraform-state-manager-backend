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

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
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
// Dashboard overview over the persistent analysis store
// ---------------------------------------------------------------------------

// TestDashboardOverview_StoreAggregation drives the full new flow end to end:
// ?refresh=true runs a statesync cycle over a real local source (sqlmock store),
// then the handler aggregates the store and reports per-source sync status.
func TestDashboardOverview_StoreAggregation(t *testing.T) {
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	// The sync cycle reads states concurrently; expectation order is not
	// deterministic across the sync + aggregate queries.
	mock.MatchExpectationsInOrder(false)
	dir := t.TempDir()

	h := NewSourcesHandlers(db, nil)
	syncer := statesync.New(
		repositories.NewSourceRepository(db),
		repositories.NewStateAnalysisRepository(db),
		ConnectSource,
	)
	h.AttachSyncer(syncer)
	r := gin.New()
	r.GET("/api/v1/dashboard/overview", h.DashboardOverview())

	env := &sourcesEnv{r: r, mock: mock, dir: dir}
	env.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web", "aws_vpc.main"))

	cfg, _ := json.Marshal(map[string]any{"base_path": dir})
	sourceListRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
			AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10")
	}

	// Sync cycle: list sources, diff markers (none yet), upsert the analysis,
	// prune nothing, record status.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceListRows())
	mock.ExpectQuery("SELECT state_key, version_marker FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "version_marker"}))
	mock.ExpectExec("INSERT INTO state_analyses").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM state_analyses WHERE source_id").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO source_sync_status").WillReturnResult(sqlmock.NewResult(0, 1))

	// Handler aggregation over the store.
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(sourceListRows())
	mock.ExpectQuery(`SELECT COUNT\(\*\),`).WillReturnRows(
		sqlmock.NewRows([]string{"count", "rum", "managed", "data", "total"}).AddRow(1, 2, 2, 0, 2))
	mock.ExpectQuery(`jsonb_each_text\(providers\)`).WillReturnRows(
		sqlmock.NewRows([]string{"key", "sum"}).AddRow("aws", 2))
	mock.ExpectQuery(`jsonb_each_text\(resource_types\)`).WillReturnRows(
		sqlmock.NewRows([]string{"key", "sum"}).AddRow("aws_instance", 1).AddRow("aws_vpc", 1))
	mock.ExpectQuery("SELECT CASE WHEN terraform_version").WillReturnRows(
		sqlmock.NewRows([]string{"v", "count"}).AddRow("1.9.5", 1))
	mock.ExpectQuery("FROM source_sync_status").WillReturnRows(
		sqlmock.NewRows([]string{"source_id", "last_sync_at", "states_listed", "read_errors", "last_error", "stored"}).
			AddRow("s1", "2026-06-11T09:00:00Z", 1, 0, "", 1))

	w := env.do(http.MethodGet, "/api/v1/dashboard/overview?refresh=true", "")
	if w.Code != http.StatusOK {
		t.Fatalf("overview: status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"rum":2`, `"states":1`, `"states_listed":1`, `"refreshed_at":"2026-06-11T09:00:00Z"`, `"source_errors":0`, `"last_sync_at":"2026-06-11T09:00:00Z"`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %s: %s", want, body)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}

	// The nil-syncer write hook is a no-op (rigs without a syncer).
	h2 := NewSourcesHandlers(db, nil)
	h2.refreshAnalysisAsync(&repositories.Source{ID: "s1"}, "app.tfstate")
}

// TestDashboardOverview_SyncStatusDegraded: a source whose last cycle had read
// errors counts toward source_errors and never-synced sources report synced=false.
func TestDashboardOverview_SyncStatusDegraded(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	h := NewSourcesHandlers(db, nil)
	r := gin.New()
	r.GET("/api/v1/dashboard/overview", h.DashboardOverview())
	env := &sourcesEnv{r: r, mock: mock}

	cfg, _ := json.Marshal(map[string]any{"base_path": "/tmp"})
	mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
			AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10").
			AddRow("s2", "fresh", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10"))
	mock.ExpectQuery(`SELECT COUNT\(\*\),`).WillReturnRows(
		sqlmock.NewRows([]string{"count", "rum", "managed", "data", "total"}).AddRow(80, 5009, 5012, 992, 6004))
	mock.ExpectQuery(`jsonb_each_text\(providers\)`).WillReturnRows(sqlmock.NewRows([]string{"key", "sum"}))
	mock.ExpectQuery(`jsonb_each_text\(resource_types\)`).WillReturnRows(sqlmock.NewRows([]string{"key", "sum"}))
	mock.ExpectQuery("SELECT CASE WHEN terraform_version").WillReturnRows(sqlmock.NewRows([]string{"v", "count"}))
	mock.ExpectQuery("FROM source_sync_status").WillReturnRows(
		sqlmock.NewRows([]string{"source_id", "last_sync_at", "states_listed", "read_errors", "last_error", "stored"}).
			AddRow("s1", "2026-06-11T09:00:00Z", 165, 3, "read ws-1: 429", 162))

	w := env.do(http.MethodGet, "/api/v1/dashboard/overview", "")
	if w.Code != http.StatusOK {
		t.Fatalf("overview: status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"source_errors":1`, `"read_errors":3`, `"synced":false`, `"states_listed":165`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %s: %s", want, body)
		}
	}
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
