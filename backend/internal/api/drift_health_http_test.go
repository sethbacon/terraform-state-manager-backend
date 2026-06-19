package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
)

func newDriftEnv(t *testing.T) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	h := NewDriftHandlers(cfg, db, nil, nil)
	hh := NewHealthHandlers(cfg, db, nil)

	r := gin.New()
	v1 := r.Group("/api/v1")
	v1.GET("/pipelines", h.ListPipelines())
	v1.POST("/pipelines", h.CreatePipeline())
	v1.PUT("/pipelines/:id", h.UpdatePipeline())
	v1.DELETE("/pipelines/:id", h.DeletePipeline())
	v1.GET("/drift/runs", h.ListRuns())
	v1.POST("/drift/runs", h.CreateRun())
	v1.GET("/drift/runs/:id", h.GetRun())
	v1.POST("/drift/runs/:id/results", h.RunResults())
	v1.GET("/drift/workflow", h.WorkflowTemplate(nil))
	v1.POST("/drift/ingest", h.IngestDrift())
	v1.GET("/drift/records", h.ListDriftRecords())
	v1.GET("/drift/records/:id", h.GetDriftRecord())
	v1.POST("/drift/records/:id/acknowledge", h.AcknowledgeDriftRecord())
	v1.POST("/drift/records/:id/resolve", h.ResolveDriftRecord())
	v1.GET("/health-lab/runs", hh.ListRuns())
	v1.POST("/health-lab/runs", hh.CreateRun())
	v1.GET("/health-lab/runs/:id", hh.GetRun())
	v1.POST("/health-lab/runs/:id/results", hh.RunResults())
	v1.GET("/health-lab/workflow", hh.WorkflowTemplate(nil))
	return &sourcesEnv{r: r, mock: mock}
}

var driftCols = []string{"id", "pipeline_connection_id", "source_id", "state_key", "repo_ref", "working_dir",
	"status", "added", "changed", "destroyed", "drifted", "summary", "detail", "callback_token", "actor",
	"created_at", "updated_at"}

func driftRow(token string) *sqlmock.Rows {
	return sqlmock.NewRows(driftCols).
		AddRow("d1", "p1", nil, "app.tfstate", "", "", "dispatched", nil, nil, nil, nil, nil, "", token, "alice",
			"2026-06-10", "2026-06-10")
}

var healthCols = []string{"id", "pipeline_connection_id", "repo_ref", "working_dir", "terraform_version",
	"provider_versions", "module_versions", "registry_host", "status", "init_ok", "plan_ok", "success",
	"summary", "detail", "callback_token", "actor", "created_at", "updated_at"}

func healthRow(token string) *sqlmock.Rows {
	return sqlmock.NewRows(healthCols).
		AddRow("h1", "p1", "", "", "", []byte(`{}`), []byte(`{}`), "", "dispatched", nil, nil, nil,
			nil, "", token, "alice", "2026-06-10", "2026-06-10")
}

func pipelineHTTPRow(t *testing.T, provider, token string, cfgMap map[string]any) *sqlmock.Rows {
	t.Helper()
	cfgJSON, _ := json.Marshal(cfgMap)
	var enc []byte
	if token != "" {
		var err error
		if enc, err = crypto.Encrypt([]byte(token)); err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
	}
	return sqlmock.NewRows([]string{"id", "name", "provider", "config", "encrypted_token", "created_at", "updated_at"}).
		AddRow("p1", "ci", provider, cfgJSON, enc, "2026-06-10", "2026-06-10")
}

func TestPipelines_CRUD(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections").
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "", map[string]any{"owner": "o"}))
	if w := e.do(http.MethodGet, "/api/v1/pipelines", ""); w.Code != http.StatusOK {
		t.Errorf("list: status = %d", w.Code)
	}

	if w := e.do(http.MethodPost, "/api/v1/pipelines", `{"name":"x"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing provider: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/pipelines", `{"name":"x","provider":"jenkins"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unsupported provider: status = %d, want 400", w.Code)
	}

	e.mock.ExpectQuery("INSERT INTO pipeline_connections").
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "tok", map[string]any{"owner": "o"}))
	w := e.do(http.MethodPost, "/api/v1/pipelines",
		`{"name":"ci","provider":"github_actions","config":{"owner":"o","repo":"r","workflow_id":"tsm-drift.yml"},"token":"ghp_x"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ghp_x") {
		t.Error("create response leaked the token")
	}

	if w := e.do(http.MethodPut, "/api/v1/pipelines/p1", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("update without name: status = %d, want 400", w.Code)
	}

	// Update edits name+config; an omitted token preserves the stored credential.
	e.mock.ExpectQuery("UPDATE pipeline_connections").
		WithArgs("p1", "renamed", `{"owner":"o","repo":"r2"}`, false, nil).
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "", map[string]any{"owner": "o", "repo": "r2"}))
	if w := e.do(http.MethodPut, "/api/v1/pipelines/p1",
		`{"name":"renamed","config":{"owner":"o","repo":"r2"}}`); w.Code != http.StatusOK {
		t.Fatalf("update: status = %d (%s)", w.Code, w.Body.String())
	}

	// Supplying a token rotates the encrypted credential; it must never echo back.
	e.mock.ExpectQuery("UPDATE pipeline_connections").
		WithArgs("p1", "renamed", `{"owner":"o"}`, true, sqlmock.AnyArg()).
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "tok", map[string]any{"owner": "o"}))
	w2 := e.do(http.MethodPut, "/api/v1/pipelines/p1",
		`{"name":"renamed","config":{"owner":"o"},"token":"ghp_rotated"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("update with token: status = %d (%s)", w2.Code, w2.Body.String())
	}
	if strings.Contains(w2.Body.String(), "ghp_rotated") {
		t.Error("update response leaked the rotated token")
	}

	// No row matched → 404.
	e.mock.ExpectQuery("UPDATE pipeline_connections").
		WillReturnError(sql.ErrNoRows)
	if w := e.do(http.MethodPut, "/api/v1/pipelines/ghost", `{"name":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("update missing: status = %d, want 404", w.Code)
	}

	e.mock.ExpectExec("DELETE FROM pipeline_connections").WithArgs("p1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/pipelines/p1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w.Code)
	}
}

func TestDriftCreateRun(t *testing.T) {
	e := newDriftEnv(t)

	if w := e.do(http.MethodPost, "/api/v1/drift/runs", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing pipeline id: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/drift/runs",
		`{"pipeline_connection_id":"p1","working_dir":"bad dir; rm -rf"}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid working_dir: status = %d, want 400", w.Code)
	}

	// Unknown pipeline → 404.
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "provider", "config", "encrypted_token", "created_at", "updated_at"}))
	if w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"ghost"}`); w.Code != http.StatusNotFound {
		t.Errorf("missing pipeline: status = %d, want 404", w.Code)
	}

	// Incomplete GitHub config: the run is recorded, the dispatch fails before
	// any network call, the run flips to failed, and the handler returns 502
	// with the run body (callback token stripped).
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "ghp_x", map[string]any{"owner": "o"}))
	e.mock.ExpectQuery("INSERT INTO drift_runs").WillReturnRows(driftRow("tok-1"))
	e.mock.ExpectExec("UPDATE drift_runs SET status").WillReturnResult(sqlmock.NewResult(0, 1))
	w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("dispatch failure: status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "workflow_id") {
		t.Errorf("dispatch error detail missing: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tok-1") {
		t.Error("response leaked the callback token")
	}

	// Pipeline without its own token resolves through its CI source; a missing
	// CI source is a hard error before dispatch.
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(pipelineHTTPRow(t, "github_actions", "", map[string]any{"ci_source_id": "c-gone"}))
	e.mock.ExpectQuery("SELECT .+ FROM ci_sources WHERE id").WithArgs("c-gone").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "provider", "organization", "project", "encrypted_token", "created_at", "updated_at"}))
	w = e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("dangling ci_source: status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
}

func TestDriftRunsReadAndCallback(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM drift_runs ORDER BY").WithArgs(50).
		WillReturnRows(driftRow("secret"))
	w := e.do(http.MethodGet, "/api/v1/drift/runs", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("list: status = %d, token leaked = %v", w.Code, strings.Contains(w.Body.String(), "secret"))
	}

	e.mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(driftRow("secret"))
	w = e.do(http.MethodGet, "/api/v1/drift/runs/d1", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("get: status = %d, token leaked = %v", w.Code, strings.Contains(w.Body.String(), "secret"))
	}

	e.mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(driftCols))
	if w := e.do(http.MethodGet, "/api/v1/drift/runs/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing run: status = %d, want 404", w.Code)
	}

	// Callback with a wrong token → uniform 401 (no existence oracle).
	e.mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(driftRow("right-token"))
	w = e.do(http.MethodPost, "/api/v1/drift/runs/d1/results", `{"token":"wrong","status":"completed"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", w.Code)
	}

	// Happy path: token matches, consumed, result recorded.
	e.mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(driftRow("right-token"))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "right-token").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE drift_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPost, "/api/v1/drift/runs/d1/results",
		`{"token":"right-token","status":"completed","added":1,"changed":0,"destroyed":0}`)
	if w.Code != http.StatusOK {
		t.Fatalf("callback: status = %d (%s)", w.Code, w.Body.String())
	}

	// Replay: token row already cleared → consume affects 0 rows → 409.
	e.mock.ExpectQuery("SELECT .+ FROM drift_runs WHERE id").WithArgs("d1").
		WillReturnRows(driftRow("right-token"))
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "right-token").
		WillReturnResult(sqlmock.NewResult(0, 0))
	w = e.do(http.MethodPost, "/api/v1/drift/runs/d1/results", `{"token":"right-token"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("replayed callback: status = %d, want 409", w.Code)
	}
}

func TestWorkflowTemplates(t *testing.T) {
	e := newDriftEnv(t)
	for _, tc := range []struct{ path, want string }{
		{"/api/v1/drift/workflow", "callback_url"},
		{"/api/v1/drift/workflow?provider=azure_devops", "callback_url"},
		{"/api/v1/health-lab/workflow", "callback_url"},
		{"/api/v1/health-lab/workflow?provider=azure_devops", "callback_url"},
	} {
		w := e.do(http.MethodGet, tc.path, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: status = %d, contains %q = %v", tc.path, w.Code, tc.want, strings.Contains(w.Body.String(), tc.want))
		}
	}
}

func TestHealthRuns(t *testing.T) {
	e := newDriftEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM health_runs ORDER BY").WithArgs(50).
		WillReturnRows(healthRow("secret"))
	w := e.do(http.MethodGet, "/api/v1/health-lab/runs", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("list: status = %d, token leaked = %v", w.Code, strings.Contains(w.Body.String(), "secret"))
	}

	// Dispatch with incomplete ADO config: recorded then failed → 502.
	e.mock.ExpectQuery("SELECT .+ FROM pipeline_connections WHERE id").WithArgs("p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat", map[string]any{"organization": "corp"}))
	e.mock.ExpectQuery("INSERT INTO health_runs").WillReturnRows(healthRow("tok"))
	e.mock.ExpectExec("UPDATE health_runs SET status").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPost, "/api/v1/health-lab/runs", `{"pipeline_connection_id":"p1"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("dispatch failure: status = %d, want 502 (%s)", w.Code, w.Body.String())
	}

	// Callback happy path mirrors drift.
	e.mock.ExpectQuery("SELECT .+ FROM health_runs WHERE id").WithArgs("h1").
		WillReturnRows(healthRow("cb-token"))
	e.mock.ExpectExec("UPDATE health_runs SET callback_token=''").WithArgs("h1", "cb-token").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE health_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.doWithHeader(http.MethodPost, "/api/v1/health-lab/runs/h1/results",
		`{"status":"completed","init_ok":true,"plan_ok":true,"success":true}`,
		"X-TSM-Callback-Token", "cb-token")
	if w.Code != http.StatusOK {
		t.Fatalf("health callback: status = %d (%s)", w.Code, w.Body.String())
	}

	// Wrong token → 401.
	e.mock.ExpectQuery("SELECT .+ FROM health_runs WHERE id").WithArgs("h1").
		WillReturnRows(healthRow("cb-token"))
	w = e.doWithHeader(http.MethodPost, "/api/v1/health-lab/runs/h1/results", `{}`,
		"X-TSM-Callback-Token", "nope")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", w.Code)
	}
}

// doWithHeader mirrors do() with one extra request header.
func (e *sourcesEnv) doWithHeader(method, path, body, header, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, value)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}
