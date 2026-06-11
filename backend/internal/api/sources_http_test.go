package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// minState renders a minimal valid Terraform state (format v4).
func minState(serial int64, lineage string, resources ...string) string {
	blocks := make([]string, 0, len(resources))
	for _, r := range resources {
		parts := strings.SplitN(r, ".", 2)
		blocks = append(blocks, fmt.Sprintf(`{
			"module": "", "mode": "managed", "type": %q, "name": %q,
			"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
			"instances": [{}]
		}`, parts[0], parts[1]))
	}
	return fmt.Sprintf(`{"version":4,"terraform_version":"1.9.5","serial":%d,"lineage":%q,"resources":[%s]}`,
		serial, lineage, strings.Join(blocks, ","))
}

// sourcesEnv is a SourcesHandlers test rig: sqlmock app DB + a REAL local
// connector rooted at a temp dir, wired through the real route shapes.
type sourcesEnv struct {
	r    *gin.Engine
	mock sqlmock.Sqlmock
	dir  string
}

func newSourcesEnv(t *testing.T) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewSourcesHandlers(db, nil) // nil identity DB: auditor is nil-safe

	r := gin.New()
	v1 := r.Group("/api/v1")
	v1.GET("/sources", h.ListSources())
	v1.POST("/sources", h.CreateSource())
	v1.GET("/sources/:id", h.GetSource())
	v1.DELETE("/sources/:id", h.DeleteSource())
	v1.GET("/sources/:id/states", h.ListStates())
	v1.GET("/sources/:id/state/analysis", h.AnalyzeState())
	v1.GET("/sources/:id/state/raw", h.RawState())
	v1.PUT("/sources/:id/state/raw", h.EditState())
	v1.GET("/sources/:id/state/resources", h.ListStateResources())
	v1.GET("/sources/:id/state/outputs", h.StateOutputs())
	v1.GET("/sources/:id/state/report", h.StateReport())
	v1.POST("/sources/:id/state/operations", h.StateOperation())
	v1.GET("/sources/:id/state/backups", h.ListBackups())
	v1.POST("/sources/:id/state/backups/:backupId/restore", h.RestoreBackup())
	v1.POST("/sources/:id/state/backup", h.BackupToSource())
	v1.POST("/sources/:id/state/migrate", h.MigrateToSource())

	return &sourcesEnv{r: r, mock: mock, dir: t.TempDir()}
}

func (e *sourcesEnv) seed(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func (e *sourcesEnv) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return string(b)
}

// expectSource queues a GetByID row for a local source rooted at dir.
func (e *sourcesEnv) expectSource(id, dir string) {
	cfg, _ := json.Marshal(map[string]any{"base_path": dir})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs(id).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
			AddRow(id, "local-"+id, "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10"))
}

func (e *sourcesEnv) do(method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// Source CRUD
// ---------------------------------------------------------------------------

func TestListSources(t *testing.T) {
	e := newSourcesEnv(t)
	cfg, _ := json.Marshal(map[string]any{"base_path": e.dir})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources ORDER BY").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
			AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10"))

	w := e.do(http.MethodGet, "/api/v1/sources", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"demo"`) {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}

	// Unexpected query → repo error → 500.
	if w := e.do(http.MethodGet, "/api/v1/sources", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("DB failure: status = %d, want 500", w.Code)
	}
}

func TestCreateSource(t *testing.T) {
	e := newSourcesEnv(t)

	if w := e.do(http.MethodPost, "/api/v1/sources", `{"type":"local"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing name: status = %d, want 400", w.Code)
	}
	// Connector validation runs before any DB work.
	if w := e.do(http.MethodPost, "/api/v1/sources", `{"name":"x","type":"local","config":{}}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid connector config: status = %d, want 400", w.Code)
	}

	cfg, _ := json.Marshal(map[string]any{"base_path": e.dir})
	e.mock.ExpectQuery("INSERT INTO state_sources").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}).
			AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10"))
	body := fmt.Sprintf(`{"name":"demo","type":"local","config":{"base_path":%q}}`, e.dir)
	if w := e.do(http.MethodPost, "/api/v1/sources", body); w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestGetAndDeleteSource(t *testing.T) {
	e := newSourcesEnv(t)

	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodGet, "/api/v1/sources/s1", ""); w.Code != http.StatusOK {
		t.Errorf("get: status = %d", w.Code)
	}

	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}))
	if w := e.do(http.MethodGet, "/api/v1/sources/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing: status = %d, want 404", w.Code)
	}

	e.mock.ExpectExec("DELETE FROM state_sources").WithArgs("s1").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/sources/s1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Read plane over a real local source
// ---------------------------------------------------------------------------

func TestListStates_RealLocalSource(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))
	e.seed(t, "net.tfstate", minState(3, "lin-2", "aws_vpc.main"))
	// Zero-byte file mimics backends whose listing has no size (HCP): the
	// handler overlays the size the analysis store recorded.
	e.seed(t, "sizeless.tfstate", "")

	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("SELECT state_key, size FROM state_analyses").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"state_key", "size"}).
			AddRow("sizeless.tfstate", 4242).
			AddRow("app.tfstate", 9)) // ignored: connector already has a size
	w := e.do(http.MethodGet, "/api/v1/sources/s1/states", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "app.tfstate") || !strings.Contains(w.Body.String(), "net.tfstate") {
		t.Errorf("states missing from listing: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"size":4242`) {
		t.Errorf("store size not overlaid on sizeless state: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"size":9`) {
		t.Errorf("overlay must not replace a real connector size: %s", w.Body.String())
	}

	// A store error is best-effort: the listing still serves.
	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("SELECT state_key, size FROM state_analyses").
		WillReturnError(errors.New("store down"))
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/states", ""); w.Code != http.StatusOK {
		t.Errorf("listing must survive a store failure: status = %d", w.Code)
	}
}

func TestStateOutputs(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", `{
		"version": 4, "lineage": "lin-1", "serial": 7,
		"outputs": {
			"vpc_id": {"value": "vpc-123", "type": "string"},
			"db_password": {"value": "hunter2", "type": "string", "sensitive": true}
		},
		"resources": []
	}`)

	e.expectSource("s1", e.dir)
	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/outputs?key=app.tfstate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"vpc_id"`) || !strings.Contains(body, `"vpc-123"`) {
		t.Errorf("plain output missing: %s", body)
	}
	if !strings.Contains(body, `"db_password"`) || !strings.Contains(body, `"sensitive":true`) {
		t.Errorf("sensitive output not listed: %s", body)
	}
	// The sensitive VALUE must never cross the API boundary.
	if strings.Contains(body, "hunter2") {
		t.Errorf("sensitive value leaked: %s", body)
	}

	// Missing key short-circuits; junk state is a 422.
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/outputs", ""); w.Code != http.StatusBadRequest {
		t.Errorf("missing key: status = %d, want 400", w.Code)
	}
	e.seed(t, "junk.tfstate", `{"hello":"world"}`)
	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/outputs?key=junk.tfstate", ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("junk state: status = %d, want 422", w.Code)
	}
}

func TestAnalyzeRawResourcesReport(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web", "aws_vpc.main"))

	// Missing ?key= short-circuits before any DB access.
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/analysis", ""); w.Code != http.StatusBadRequest {
		t.Errorf("missing key: status = %d, want 400", w.Code)
	}

	e.expectSource("s1", e.dir)
	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/analysis?key=app.tfstate", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"rum":2`) {
		t.Fatalf("analysis: status = %d (%s)", w.Code, w.Body.String())
	}

	e.expectSource("s1", e.dir)
	w = e.do(http.MethodGet, "/api/v1/sources/s1/state/raw?key=app.tfstate", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"serial":7`) {
		t.Fatalf("raw: status = %d", w.Code)
	}

	e.expectSource("s1", e.dir)
	w = e.do(http.MethodGet, "/api/v1/sources/s1/state/resources?key=app.tfstate", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "aws_instance") {
		t.Fatalf("resources: status = %d (%s)", w.Code, w.Body.String())
	}

	for _, format := range []string{"json", "md", "csv"} {
		// The identity DB is nil here, so the auditor no-ops — no audit INSERT.
		e.expectSource("s1", e.dir)
		w = e.do(http.MethodGet, "/api/v1/sources/s1/state/report?key=app.tfstate&format="+format, "")
		if w.Code != http.StatusOK {
			t.Fatalf("report %s: status = %d (%s)", format, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
			t.Errorf("report %s: missing attachment disposition", format)
		}
	}

	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/report?key=app.tfstate&format=pdf", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad format: status = %d, want 400", w.Code)
	}

	// Unreadable key → upstream (connector) error.
	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/raw?key=missing.tfstate", ""); w.Code != http.StatusBadGateway {
		t.Errorf("missing state file: status = %d, want 502", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Edit plane: lock → backup → write → audit against the real filesystem
// ---------------------------------------------------------------------------

func TestEditState_BackupThenWrite(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))

	newState := minState(8, "lin-1", "aws_instance.web")
	w := e.do(http.MethodPut, "/api/v1/sources/s1/state/raw?key=app.tfstate", newState)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(e.read(t, "app.tfstate"), `"serial":8`) {
		t.Error("edit did not write the new state to disk")
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("edit must back up and audit-trail: %v", err)
	}
}

func TestEditState_SerialRegressionConflicts(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	w := e.do(http.MethodPut, "/api/v1/sources/s1/state/raw?key=app.tfstate", minState(3, "lin-1", "aws_instance.web"))
	if w.Code != http.StatusConflict {
		t.Fatalf("serial regression: status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(e.read(t, "app.tfstate"), `"serial":7`) {
		t.Error("conflicting edit must not touch the file")
	}

	// force=true overrides, still backing up first.
	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b2"))
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPut, "/api/v1/sources/s1/state/raw?key=app.tfstate&force=true", minState(3, "lin-1", "aws_instance.web"))
	if w.Code != http.StatusOK {
		t.Fatalf("forced edit: status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestEditState_LineageMismatchConflicts(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	w := e.do(http.MethodPut, "/api/v1/sources/s1/state/raw?key=app.tfstate", minState(8, "DIFFERENT", "aws_instance.web"))
	if w.Code != http.StatusConflict {
		t.Errorf("lineage mismatch: status = %d, want 409", w.Code)
	}
}

func TestEditState_RejectsInvalidState(t *testing.T) {
	e := newSourcesEnv(t)
	if w := e.do(http.MethodPut, "/api/v1/sources/s1/state/raw?key=k", "not json"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid state: status = %d, want 422", w.Code)
	}
	if w := e.do(http.MethodPut, "/api/v1/sources/s1/state/raw", "{}"); w.Code != http.StatusBadRequest {
		t.Errorf("missing key: status = %d, want 400", w.Code)
	}
}

func TestStateOperation_RemoveResource(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web", "aws_vpc.main"))

	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"rm","address":"aws_vpc.main"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rm: status = %d (%s)", w.Code, w.Body.String())
	}
	after := e.read(t, "app.tfstate")
	if strings.Contains(after, "aws_vpc") || !strings.Contains(after, "aws_instance") {
		t.Errorf("rm did not remove exactly the addressed resource: %s", after)
	}
}

func TestStateOperation_Validation(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=k", `{"op":"rm"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing address: status = %d, want 400", w.Code)
	}

	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"mv","address":"aws_instance.web"}`); w.Code != http.StatusBadRequest {
		t.Errorf("mv without to: status = %d, want 400", w.Code)
	}

	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"explode","address":"x.y"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown op: status = %d, want 400", w.Code)
	}

	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"rm","address":"aws_db.none"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown address: status = %d, want 422", w.Code)
	}
}

func TestListAndRestoreBackups(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(9, "lin-1", "aws_instance.web"))

	e.mock.ExpectQuery("FROM state_backups").WithArgs("s1", "app.tfstate").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_id", "state_key", "serial", "created_by", "created_at"}).
			AddRow("b1", "s1", "app.tfstate", 7, "alice", "2026-06-10"))
	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/backups?key=app.tfstate", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"b1"`) {
		t.Fatalf("list backups: status = %d (%s)", w.Code, w.Body.String())
	}

	// Restore b1 (serial 7 content) over the current serial-9 file.
	backupData := minState(7, "lin-1", "aws_instance.web")
	e.mock.ExpectQuery("FROM state_backups WHERE id").WithArgs("b1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_id", "state_key", "data", "serial", "created_by", "created_at"}).
			AddRow("b1", "s1", "app.tfstate", []byte(backupData), 7, "alice", "2026-06-10"))
	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b2")) // pre-restore safety backup
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))

	w = e.do(http.MethodPost, "/api/v1/sources/s1/state/backups/b1/restore", "")
	if w.Code != http.StatusOK {
		t.Fatalf("restore: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(e.read(t, "app.tfstate"), `"serial":7`) {
		t.Error("restore did not write the backup content")
	}

	// Backup belonging to another source is rejected.
	e.mock.ExpectQuery("FROM state_backups WHERE id").WithArgs("b9").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_id", "state_key", "data", "serial", "created_by", "created_at"}).
			AddRow("b9", "OTHER", "app.tfstate", []byte(backupData), 7, "alice", "2026-06-10"))
	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backups/b9/restore", ""); w.Code != http.StatusBadRequest {
		t.Errorf("cross-source restore: status = %d, want 400", w.Code)
	}

	e.mock.ExpectQuery("FROM state_backups WHERE id").WithArgs("ghost").WillReturnError(sql.ErrNoRows)
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backups/ghost/restore", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing backup: status = %d, want 404", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Transfer plane: two real local sources
// ---------------------------------------------------------------------------

func TestTransfer_BackupAndMigrate(t *testing.T) {
	e := newSourcesEnv(t)
	dirB := t.TempDir()
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	transferCols := []string{"id", "mode", "source_id", "source_key", "target_source_id", "target_key", "status", "verified", "decommissioned", "detail", "actor", "created_at"}

	// backup: A → B, A untouched.
	e.expectSource("s1", e.dir)
	e.expectSource("s2", dirB)
	e.mock.ExpectQuery("INSERT INTO state_transfers").
		WillReturnRows(sqlmock.NewRows(transferCols).
			AddRow("t1", "backup", "s1", "app.tfstate", "s2", "copy.tfstate", "success", true, false, "", "", "2026-06-10"))
	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"s2","target_key":"copy.tfstate"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("backup transfer: status = %d (%s)", w.Code, w.Body.String())
	}
	if b, err := os.ReadFile(filepath.Join(dirB, "copy.tfstate")); err != nil || !strings.Contains(string(b), `"serial":7`) {
		t.Errorf("backup did not copy state to the target: %v", err)
	}
	if !strings.Contains(e.read(t, "app.tfstate"), `"serial":7`) {
		t.Error("backup must not touch the source")
	}

	// Validation errors.
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=k", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing target: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup", `{"target_source_id":"s2","target_key":"k"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing key: status = %d, want 400", w.Code)
	}

	// Missing target source → 404.
	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type", "endpoint", "config", "scope", "encrypted_credentials", "created_at", "updated_at"}))
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"ghost","target_key":"k"}`); w.Code != http.StatusNotFound {
		t.Errorf("missing target source: status = %d, want 404", w.Code)
	}
}
