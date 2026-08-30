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
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// testActingOrg is the organization the rig acts in. A caller with exactly one
// never has to send the header, so the create paths resolve it implicitly.
const testActingOrg = "11111111-1111-4111-8111-111111111111"

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
	r      *gin.Engine
	mock   sqlmock.Sqlmock
	dir    string
	scopes []string // caller scopes injected into the gin context (nil = none)
}

func newSourcesEnv(t *testing.T) *sourcesEnv {
	return newSourcesEnvWithScope(t, tenantscope.Scope{OrgIDs: []string{testActingOrg}})
}

// newSourcesEnvWithScope builds the rig with an explicit scope, so a test can put
// the caller in two organizations at once -- which is what makes a transfer
// ACROSS the partition boundary an authorized act rather than a refused one.
// newSourcesEnvWithoutScope builds the rig with NO tenant scope stored, standing
// in for a route whose middleware.TenantScope was never wired. A mutating route
// must treat that as a fault rather than carrying on unscoped.
func newSourcesEnvWithoutScope(t *testing.T) *sourcesEnv {
	return newSourcesEnvScoped(t, nil)
}

func newSourcesEnvWithScope(t *testing.T, scope tenantscope.Scope) *sourcesEnv {
	return newSourcesEnvScoped(t, &scope)
}

func newSourcesEnvScoped(t *testing.T, scope *tenantscope.Scope) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	// newSQLMock, not sqlmock.New: it installs pgxparam.Converter, without which
	// a []string argument is not a valid driver.Value and the call fails at the
	// driver before any expectation is consulted. Production runs on pgx, which
	// takes a []string for `= ANY($n)` — so this rig could not exercise a scoped
	// statement at all until now. Nothing here had passed one before.
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewSourcesHandlers(db, nil) // nil identity DB: auditor is nil-safe

	env := &sourcesEnv{mock: mock, dir: t.TempDir()}
	r := gin.New()
	// Inject caller scopes (set by the auth middleware in production) so handlers
	// that branch on scope — e.g. the admin-only state delete — are testable.
	r.Use(func(c *gin.Context) {
		if env.scopes != nil {
			c.Set("scopes", env.scopes)
		}
		// What middleware.TenantScope publishes in production. Stored directly
		// rather than resolved, so this rig needs no membership store — but it
		// must be stored, because a route that CREATES treats an unresolved
		// scope as a 500 rather than as "no memberships" (#436). Omitting it
		// here would make every create test fail with the message that says the
		// route was never wired, which is precisely the distinction worth
		// keeping.
		if scope != nil {
			tenantscope.Store(c, *scope)
		}
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/sources", h.ListSources())
	v1.POST("/sources", h.CreateSource())
	v1.GET("/sources/:id", h.GetSource())
	v1.PUT("/sources/:id", h.UpdateSource())
	v1.DELETE("/sources/:id", h.DeleteSource())
	v1.POST("/sources/test", h.TestSourceConfig())
	v1.POST("/sources/:id/test", h.TestSource())
	v1.GET("/sources/:id/states", h.ListStates())
	v1.GET("/sources/:id/state/analysis", h.AnalyzeState())
	v1.GET("/sources/:id/state/raw", h.RawState())
	v1.PUT("/sources/:id/state/raw", h.EditState())
	v1.POST("/sources/:id/state/diff", h.EditDiff())
	v1.GET("/sources/:id/state/resources", h.ListStateResources())
	v1.GET("/sources/:id/state/outputs", h.StateOutputs())
	v1.GET("/sources/:id/state/history", h.StateHistory())
	v1.GET("/sources/:id/state/report", h.StateReport())
	v1.POST("/sources/:id/state/operations", h.StateOperation())
	v1.GET("/sources/:id/state/backups", h.ListBackups())
	v1.GET("/sources/:id/state/backups/:backupId", h.GetBackupContent())
	v1.GET("/sources/:id/state/backups/:backupId/diff", h.DiffBackup())
	v1.POST("/sources/:id/state/backups/:backupId/restore", h.RestoreBackup())
	v1.GET("/sources/:id/state/locks", h.ListLocks())
	v1.DELETE("/sources/:id/state/lock", h.ForceUnlock())
	v1.POST("/sources/:id/state/backup", h.BackupToSource())
	v1.POST("/sources/:id/state/migrate", h.MigrateToSource())
	// The transfer RECORD read, which is a partition root of its own and now
	// resolves a scope (#393 Phase 3). Registered here rather than only in
	// router.go so the refusal has a rig to be asserted in.
	v1.GET("/transfers/:id", h.GetTransfer())

	env.r = r
	return env
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
	// SCOPED shape: the state plane resolves its source through GetByIDInScope,
	// which binds the organization array FIRST and the id second. A fixture still
	// scripting the by-id-alone lookup would be scripting a statement the code no
	// longer emits.
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs(sqlmock.AnyArg(), id).
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow(id, "local-"+id, "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10", testActingOrg))
}

// expectSourceUnscoped scripts the BY-ID lookup, for the handlers that still
// serve the unscoped answer on purpose — GetSource and ListSources are Phase 2b
// dual-read sites, and flipping them is gated on the estate being re-owned.
//
// The two helpers exist separately because the two shapes are now genuinely
// different statements, and a single tolerant fixture would stop reporting which
// one a handler actually emits.
func (e *sourcesEnv) expectSourceUnscoped(id, dir string) {
	cfg, _ := json.Marshal(map[string]any{"base_path": dir})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE id").WithArgs(id).
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow(id, "local-"+id, "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10", testActingOrg))
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
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10", testActingOrg))
	// The listing is paginated, so the handler also asks for the total (#282).
	e.mock.ExpectQuery("SELECT count.+WHERE organization_id").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

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
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10", testActingOrg))
	body := fmt.Sprintf(`{"name":"demo","type":"local","config":{"base_path":%q}}`, e.dir)
	if w := e.do(http.MethodPost, "/api/v1/sources", body); w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestGetAndDeleteSource(t *testing.T) {
	e := newSourcesEnv(t)

	// SCOPED now (#393 Phase 3 / #459): GetSource resolves through
	// sourceInScope, which binds the organization array first and the id second.
	// The unscoped fixture would be scripting a statement the handler no longer
	// emits.
	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodGet, "/api/v1/sources/s1", ""); w.Code != http.StatusOK {
		t.Errorf("get: status = %d", w.Code)
	}

	// A source outside the scope is indistinguishable from one that does not
	// exist, which is the point: 404 rather than 403 tells the caller nothing
	// about another organization's rows.
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs(sqlmock.AnyArg(), "ghost").
		WillReturnRows(sqlmock.NewRows(apiSourceCols))
	if w := e.do(http.MethodGet, "/api/v1/sources/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing: status = %d, want 404", w.Code)
	}

	// The organization list is bound alongside the id: the DELETE is scoped, so a
	// source in another organization is unreachable rather than merely unlisted.
	e.mock.ExpectExec(`DELETE FROM state_sources[\s\S]*organization_id`).
		WithArgs("s1", []string{testActingOrg}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/sources/s1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204 (%s)", w.Code, w.Body.String())
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

func TestUpdateSource(t *testing.T) {
	e := newSourcesEnv(t)

	// Happy path: load existing, validate new config, update; blank
	// credentials pass NULL so the stored secret is kept.
	e.expectSource("s1", e.dir)
	cfg, _ := json.Marshal(map[string]any{"base_path": e.dir})
	e.mock.ExpectQuery(`UPDATE state_sources SET[\s\S]*organization_id`).
		WithArgs("s1", "renamed", nil, sqlmock.AnyArg(), sqlmock.AnyArg(), nil, []string{testActingOrg}).
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "renamed", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-11", testActingOrg))
	body := fmt.Sprintf(`{"name":"renamed","config":{"base_path":%q}}`, e.dir)
	w := e.do(http.MethodPut, "/api/v1/sources/s1", body)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"renamed"`) {
		t.Fatalf("update: status = %d (%s)", w.Code, w.Body.String())
	}

	// Connector validation rejects a bad config before any write.
	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodPut, "/api/v1/sources/s1", `{"name":"x","config":{"base_path":"/does/not/exist"}}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid config: status = %d, want 400", w.Code)
	}

	// Missing name -> 400; unknown id -> 404.
	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodPut, "/api/v1/sources/s1", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing name: status = %d, want 400", w.Code)
	}
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs(sqlmock.AnyArg(), "ghost").
		WillReturnRows(sqlmock.NewRows(apiSourceCols))
	if w := e.do(http.MethodPut, "/api/v1/sources/ghost", `{"name":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("ghost: status = %d, want 404", w.Code)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestTestSource(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	w := e.do(http.MethodPost, "/api/v1/sources/s1/test", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) ||
		!strings.Contains(w.Body.String(), `"states":1`) {
		t.Fatalf("test ok: status = %d (%s)", w.Code, w.Body.String())
	}

	// A broken backend surfaces as 502 with the connector error.
	cfg, _ := json.Marshal(map[string]any{"base_path": filepath.Join(e.dir, "gone")})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs(sqlmock.AnyArg(), "s2").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s2", "broken", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10", testActingOrg))
	if w := e.do(http.MethodPost, "/api/v1/sources/s2/test", ""); w.Code != http.StatusBadRequest && w.Code != http.StatusBadGateway {
		t.Errorf("broken: status = %d, want 400/502", w.Code)
	}
}

func TestTestSourceConfig(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	// A valid unsaved config connects and counts states without persisting.
	body := fmt.Sprintf(`{"type":"local","config":{"base_path":%q}}`, filepath.ToSlash(e.dir))
	w := e.do(http.MethodPost, "/api/v1/sources/test", body)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) ||
		!strings.Contains(w.Body.String(), `"states":1`) {
		t.Fatalf("test ok: status = %d (%s)", w.Code, w.Body.String())
	}

	// Missing type and unknown type are config errors, not gateway errors.
	if w := e.do(http.MethodPost, "/api/v1/sources/test", `{"config":{}}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing type: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/sources/test", `{"type":"nonexistent-xyz"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown type: status = %d, want 400", w.Code)
	}

	// A bad config surfaces the connector's own error.
	badBody := fmt.Sprintf(`{"type":"local","config":{"base_path":%q}}`, filepath.ToSlash(filepath.Join(e.dir, "gone")))
	if w := e.do(http.MethodPost, "/api/v1/sources/test", badBody); w.Code != http.StatusBadRequest && w.Code != http.StatusBadGateway {
		t.Errorf("bad config: status = %d, want 400/502", w.Code)
	}
}

func TestTestSourceConfigMergesStoredCredentials(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(3, "lin-2", "aws_instance.db"))

	// source_id with blank credentials loads the stored source and reuses its
	// decrypted credentials — the edit-dialog contract (blank = keep existing).
	e.expectSource("s1", e.dir)
	body := fmt.Sprintf(`{"type":"local","config":{"base_path":%q},"source_id":"s1"}`, filepath.ToSlash(e.dir))
	w := e.do(http.MethodPost, "/api/v1/sources/test", body)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"states":1`) {
		t.Fatalf("merge ok: status = %d (%s)", w.Code, w.Body.String())
	}

	// An unknown source_id is a 404, mirroring the by-id routes.
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs(sqlmock.AnyArg(), "ghost").
		WillReturnRows(sqlmock.NewRows(apiSourceCols))
	ghost := fmt.Sprintf(`{"type":"local","config":{"base_path":%q},"source_id":"ghost"}`, filepath.ToSlash(e.dir))
	if w := e.do(http.MethodPost, "/api/v1/sources/test", ghost); w.Code != http.StatusNotFound {
		t.Errorf("ghost source_id: status = %d, want 404", w.Code)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
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

func TestStateHistory(t *testing.T) {
	e := newSourcesEnv(t)

	histCols := []string{"source_id", "state_key", "version_marker", "size", "terraform_version",
		"serial", "lineage", "rum", "managed_resources", "data_sources", "total_resources",
		"providers", "resource_types", "analyzed_at"}
	// The route resolves its SOURCE first now (#459): state_analysis_history has
	// no organization_id, so authorising the parent is what authorises the
	// history. Without this the handler 404s before reaching the query below.
	e.expectSource("s1", t.TempDir())
	e.mock.ExpectQuery("FROM state_analysis_history").WithArgs("s1", "app.tfstate", 200).
		WillReturnRows(sqlmock.NewRows(histCols).
			AddRow("s1", "app.tfstate", "12|y", 12, "1.9.5", 8, "lin", 5, 5, 1, 6,
				[]byte(`{"aws":5}`), []byte(`{"aws_instance":5}`), "2026-06-11T10:00:00Z"))
	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/history?key=app.tfstate", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"serial":8`) {
		t.Fatalf("history: %d (%s)", w.Code, w.Body.String())
	}

	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/history", ""); w.Code != http.StatusBadRequest {
		t.Errorf("missing key: %d", w.Code)
	}

	e.mock.ExpectQuery("FROM state_analysis_history").WillReturnError(errors.New("db down"))
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/history?key=k", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("db error: %d", w.Code)
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

	// force=true overrides, still backing up first, and the edit-ledger row must
	// carry the forced-override marker (#280) so a bypass of the serial/lineage
	// guards is distinguishable from an ordinary guarded write.
	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b2"))
	e.mock.ExpectExec("INSERT INTO state_edits").
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"success", "forced: serial/lineage checks overridden",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPut, "/api/v1/sources/s1/state/raw?key=app.tfstate&force=true", minState(3, "lin-1", "aws_instance.web"))
	if w.Code != http.StatusOK {
		t.Fatalf("forced edit: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("forced edit must record the override marker: %v", err)
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

func TestEditState_AbortsWhenReadFails(t *testing.T) {
	e := newSourcesEnv(t)

	// An http source whose backend 500s on read: the pre-write read cannot
	// distinguish "no state yet" from "backend down", so the edit must abort
	// (502) BEFORE writing — a transient failure must not silently skip the
	// backup and serial/lineage checks.
	var wrote bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			wrote = true
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(map[string]any{"address": srv.URL})
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs(sqlmock.AnyArg(), "s1").
		WillReturnRows(sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "flaky-http", "http", "", cfg, []byte(`{}`), nil, "2026-06-11", "2026-06-11", testActingOrg))
	// No lock_address => app-level DB lock: TTL reap, acquire, then release.
	e.mock.ExpectExec("DELETE FROM state_locks").WillReturnResult(sqlmock.NewResult(0, 0))
	e.mock.ExpectQuery("INSERT INTO state_locks").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("lk1"))
	e.mock.ExpectExec("DELETE FROM state_locks").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPut, "/api/v1/sources/s1/state/raw?key=default", minState(2, "lin-1"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot verify current state") {
		t.Errorf("error should explain the aborted guard: %s", w.Body.String())
	}
	if wrote {
		t.Error("write must not reach the backend when the pre-write read fails")
	}
}

func TestRestoreBackup_AbortsWhenPreBackupFails(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(9, "lin-1", "aws_instance.web"))

	backupData := minState(7, "lin-1", "aws_instance.web")
	e.mock.ExpectQuery("FROM state_backups WHERE id").WithArgs("b1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "source_id", "state_key", "data", "serial", "created_by", "created_at"}).
			AddRow("b1", "s1", "app.tfstate", []byte(backupData), 7, "alice", "2026-06-10"))
	e.expectSource("s1", e.dir)
	// The pre-restore safety backup fails: the restore must abort rather than
	// proceed with the current state unrecoverable.
	e.mock.ExpectQuery("INSERT INTO state_backups").WillReturnError(errors.New("db down"))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backups/b1/restore", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(e.read(t, "app.tfstate"), `"serial":9`) {
		t.Error("failed pre-restore backup must abort before writing")
	}
}

func TestForceUnlock(t *testing.T) {
	e := newSourcesEnv(t)

	e.mock.ExpectExec("DELETE FROM state_locks").WithArgs("s1", "app.tfstate").
		WillReturnResult(sqlmock.NewResult(0, 1))
	w := e.do(http.MethodDelete, "/api/v1/sources/s1/state/lock?key=app.tfstate", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"released":true`) {
		t.Fatalf("force unlock: %d (%s)", w.Code, w.Body.String())
	}

	// Nothing held is reported, not errored.
	e.mock.ExpectExec("DELETE FROM state_locks").WithArgs("s1", "other").
		WillReturnResult(sqlmock.NewResult(0, 0))
	w = e.do(http.MethodDelete, "/api/v1/sources/s1/state/lock?key=other", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"released":false`) {
		t.Fatalf("no-op force unlock: %d (%s)", w.Code, w.Body.String())
	}

	if w := e.do(http.MethodDelete, "/api/v1/sources/s1/state/lock", ""); w.Code != http.StatusBadRequest {
		t.Errorf("missing key: status = %d, want 400", w.Code)
	}
}

func TestListLocks(t *testing.T) {
	e := newSourcesEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM state_locks WHERE source_id").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_id", "state_key", "actor", "acquired_at"}).
			AddRow("l1", "s1", "app.tfstate", "user-1", "2026-07-10T08:00:00Z").
			AddRow("l2", "s1", "net.tfstate", "", "2026-07-10T07:00:00Z"))

	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/locks", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list locks: status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"app.tfstate"`, `"net.tfstate"`, `"user-1"`, `"acquired_at"`} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %s: %s", want, body)
		}
	}
}

func TestListLocks_Empty(t *testing.T) {
	e := newSourcesEnv(t)

	e.mock.ExpectQuery("SELECT .+ FROM state_locks WHERE source_id").WithArgs("s1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_id", "state_key", "actor", "acquired_at"}))

	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/locks", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"locks":[]`) {
		t.Fatalf("empty list must be [], not null: %d (%s)", w.Code, w.Body.String())
	}

	// Unexpected query → repo error → 500.
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/locks", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("DB failure: status = %d, want 500", w.Code)
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

func TestStateOperation_RemoveInstance(t *testing.T) {
	e := newSourcesEnv(t)
	// A for_each resource with three instances keyed a/b/c.
	forEach := `{"version":4,"terraform_version":"1.9.5","serial":3,"lineage":"lin-1","resources":[
		{"module":"","mode":"managed","type":"aws_prefix_list","name":"this",
		 "provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","each":"map",
		 "instances":[{"index_key":"a"},{"index_key":"b"},{"index_key":"c"}]}
	]}`
	e.seed(t, "app.tfstate", forEach)

	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"rm","address":"aws_prefix_list.this[\"a\"]"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rm instance: status = %d (%s)", w.Code, w.Body.String())
	}
	// Only instance "a" is removed; the resource block and instances b/c remain.
	after := e.read(t, "app.tfstate")
	if strings.Contains(after, `"index_key":"a"`) {
		t.Errorf("instance [\"a\"] should have been removed: %s", after)
	}
	if !strings.Contains(after, `"index_key":"b"`) || !strings.Contains(after, `"index_key":"c"`) {
		t.Errorf("instances [\"b\"] and [\"c\"] should remain: %s", after)
	}
	if !strings.Contains(after, "aws_prefix_list") {
		t.Errorf("resource block should remain: %s", after)
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

func TestStateOperation_Delete_AdminAllowed(t *testing.T) {
	e := newSourcesEnv(t)
	e.scopes = []string{"admin"}
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"delete","key":"app.tfstate"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: status = %d (%s)", w.Code, w.Body.String())
	}
	// A final backup is recorded and referenced, and the live object is gone.
	if !strings.Contains(w.Body.String(), `"deleted"`) || !strings.Contains(w.Body.String(), `"backup_id":"b1"`) {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(e.dir, "app.tfstate")); !os.IsNotExist(err) {
		t.Errorf("state file should be gone, stat err = %v", err)
	}
}

func TestStateOperation_Delete_Purge(t *testing.T) {
	e := newSourcesEnv(t)
	e.scopes = []string{"admin"}
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	e.mock.ExpectExec("DELETE FROM state_backups").WillReturnResult(sqlmock.NewResult(0, 3))
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"delete","key":"app.tfstate","purge":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"purged":true`) {
		t.Fatalf("purge delete: status = %d (%s)", w.Code, w.Body.String())
	}
	// The just-created backup is included in the purge, so no id is surfaced.
	if strings.Contains(w.Body.String(), "backup_id") {
		t.Errorf("purge response must not reference a backup id: %s", w.Body.String())
	}
}

func TestStateOperation_Delete_BackupFailureAborts(t *testing.T) {
	e := newSourcesEnv(t)
	e.scopes = []string{"admin"}
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	// The pre-delete backup fails: the object must NOT be deleted.
	e.mock.ExpectQuery("INSERT INTO state_backups").WillReturnError(errDBForTest())

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"delete","key":"app.tfstate"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("backup failure: status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(e.dir, "app.tfstate")); err != nil {
		t.Errorf("state must survive a failed pre-delete backup: %v", err)
	}
}

func TestStateOperation_Delete_PurgeFailureWarns(t *testing.T) {
	e := newSourcesEnv(t)
	e.scopes = []string{"admin"}
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))

	e.expectSource("s1", e.dir)
	e.mock.ExpectQuery("INSERT INTO state_backups").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("b1"))
	// The object delete succeeds, but dropping the backups fails: the response
	// reports success with a warning and records purged=false.
	e.mock.ExpectExec("DELETE FROM state_backups").WillReturnError(errDBForTest())
	e.mock.ExpectExec("INSERT INTO state_edits").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"delete","key":"app.tfstate","purge":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"purged":false`) {
		t.Fatalf("purge failure: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "could not be purged") {
		t.Errorf("expected a purge warning: %s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(e.dir, "app.tfstate")); !os.IsNotExist(err) {
		t.Errorf("state object should be deleted even when purge fails")
	}
}

func TestStateOperation_Delete_NonAdminForbidden(t *testing.T) {
	e := newSourcesEnv(t)
	e.scopes = []string{"state:write"} // editor, not admin
	// No expectSource: the admin gate must reject before any backend lookup.
	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"delete","key":"app.tfstate"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin delete: status = %d, want 403 (%s)", w.Code, w.Body.String())
	}
}

func TestStateOperation_Delete_KeyEchoRequired(t *testing.T) {
	e := newSourcesEnv(t)
	e.scopes = []string{"admin"}
	// No expectSource: the key-echo check must reject before any backend lookup.
	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"delete","key":"wrong.tfstate"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("key echo mismatch: status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

func TestStateOperation_Delete_MissingKey(t *testing.T) {
	e := newSourcesEnv(t)
	e.scopes = []string{"admin"}
	e.expectSource("s1", e.dir)
	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=ghost.tfstate",
		`{"op":"delete","key":"ghost.tfstate"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing key delete: status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
}

func TestStateOperation_Delete_LockedRefused(t *testing.T) {
	e := newSourcesEnv(t)
	e.scopes = []string{"admin"}
	e.seed(t, "app.tfstate", minState(7, "lin-1", "aws_instance.web"))
	// Pre-create the native lock file so the connector reports the key as locked.
	if err := os.WriteFile(filepath.Join(e.dir, "app.tfstate.tsmlock"), []byte("held"), 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	e.expectSource("s1", e.dir)
	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/operations?key=app.tfstate",
		`{"op":"delete","key":"app.tfstate"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("locked delete: status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(e.dir, "app.tfstate")); err != nil {
		t.Errorf("state file must survive a refused delete: %v", err)
	}
}

func TestListAndRestoreBackups(t *testing.T) {
	e := newSourcesEnv(t)
	e.seed(t, "app.tfstate", minState(9, "lin-1", "aws_instance.web"))

	e.mock.ExpectQuery("FROM state_backups").WithArgs("s1", "app.tfstate", 100, 0).
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

// expectBackupRow queues a GetBackup row carrying full state data.
func (e *sourcesEnv) expectBackupRow(backupID, sourceID, key, data string, serial int64) {
	e.mock.ExpectQuery("FROM state_backups WHERE id").WithArgs(backupID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_id", "state_key", "data", "serial", "created_by", "created_at"}).
			AddRow(backupID, sourceID, key, []byte(data), serial, "alice", "2026-06-10"))
}

func TestGetBackupContent(t *testing.T) {
	e := newSourcesEnv(t)
	backupData := minState(7, "lin-1", "aws_instance.web")

	e.expectBackupRow("b1", "s1", "app.tfstate", backupData, 7)
	e.expectSource("s1", e.dir)
	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/backups/b1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get backup: status = %d (%s)", w.Code, w.Body.String())
	}
	if w.Body.String() != backupData {
		t.Errorf("body must be the exact stored bytes, got %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	// Cross-source access reads as not-found (no existence leak).
	e.expectBackupRow("b9", "OTHER", "app.tfstate", backupData, 7)
	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/backups/b9", ""); w.Code != http.StatusNotFound {
		t.Errorf("cross-source read: status = %d, want 404", w.Code)
	}

	e.mock.ExpectQuery("FROM state_backups WHERE id").WithArgs("ghost").WillReturnError(sql.ErrNoRows)
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/backups/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing backup: status = %d, want 404", w.Code)
	}
}

func TestEditDiff(t *testing.T) {
	e := newSourcesEnv(t)
	// Current state: web (2 instances) + vpc. Draft: web (1 instance) + s3.
	// Saving the draft would add s3, drop vpc, and change web's instance count.
	current := `{"version":4,"terraform_version":"1.9.5","serial":9,"lineage":"lin-1","resources":[
		{"module":"","mode":"managed","type":"aws_instance","name":"web",
		 "provider":"provider[\"registry.terraform.io/hashicorp/aws\"]",
		 "instances":[{"index_key":0},{"index_key":1}]},
		{"module":"","mode":"managed","type":"aws_vpc","name":"main",
		 "provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","instances":[{}]}
	]}`
	draft := minState(7, "lin-1", "aws_instance.web", "aws_s3_bucket.logs")
	e.seed(t, "app.tfstate", current)

	e.expectSource("s1", e.dir)
	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/diff?key=app.tfstate", draft)
	if w.Code != http.StatusOK {
		t.Fatalf("diff: status = %d (%s)", w.Code, w.Body.String())
	}
	var diff struct {
		DraftSerial   *int64 `json:"draft_serial"`
		CurrentSerial *int64 `json:"current_serial"`
		Added         []struct {
			Type string `json:"type"`
		} `json:"added"`
		Removed []struct {
			Type string `json:"type"`
		} `json:"removed"`
		Changed []struct {
			Type string `json:"type"`
		} `json:"changed"`
		ApproximateChanged bool `json:"approximate_changed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &diff); err != nil {
		t.Fatalf("invalid diff json: %v (%s)", err, w.Body.String())
	}
	if diff.DraftSerial == nil || *diff.DraftSerial != 7 || diff.CurrentSerial == nil || *diff.CurrentSerial != 9 {
		t.Errorf("serials = %v/%v, want draft 7 / current 9", diff.DraftSerial, diff.CurrentSerial)
	}
	if len(diff.Added) != 1 || diff.Added[0].Type != "aws_s3_bucket" {
		t.Errorf("added = %+v, want the draft-only aws_s3_bucket", diff.Added)
	}
	if len(diff.Removed) != 1 || diff.Removed[0].Type != "aws_vpc" {
		t.Errorf("removed = %+v, want the current-only aws_vpc", diff.Removed)
	}
	if len(diff.Changed) != 1 || diff.Changed[0].Type != "aws_instance" {
		t.Errorf("changed = %+v, want aws_instance (instance-count delta)", diff.Changed)
	}
	if !diff.ApproximateChanged {
		t.Error("approximate_changed must be true (instance-level heuristic)")
	}
}

func TestEditDiff_CurrentMissing(t *testing.T) {
	e := newSourcesEnv(t)
	// No seeded file: the connector reports not-found, so saving the draft would
	// create everything - the whole draft lands in "added" and current_serial is nil.
	draft := minState(7, "lin-1", "aws_instance.web")
	e.expectSource("s1", e.dir)
	w := e.do(http.MethodPost, "/api/v1/sources/s1/state/diff?key=ghost.tfstate", draft)
	if w.Code != http.StatusOK {
		t.Fatalf("diff: status = %d (%s)", w.Code, w.Body.String())
	}
	var diff struct {
		CurrentSerial *int64 `json:"current_serial"`
		Added         []struct {
			Type string `json:"type"`
		} `json:"added"`
		Removed []json.RawMessage `json:"removed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &diff); err != nil {
		t.Fatalf("invalid diff json: %v (%s)", err, w.Body.String())
	}
	if diff.CurrentSerial != nil {
		t.Errorf("current_serial = %v, want nil (no current state)", *diff.CurrentSerial)
	}
	if len(diff.Added) != 1 || diff.Added[0].Type != "aws_instance" {
		t.Errorf("added = %+v, want the whole draft", diff.Added)
	}
	if len(diff.Removed) != 0 {
		t.Errorf("removed = %+v, want empty (nothing to drop)", diff.Removed)
	}
}

func TestEditDiff_Validation(t *testing.T) {
	e := newSourcesEnv(t)
	// Missing key -> 400.
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/diff", minState(1, "lin-1")); w.Code != http.StatusBadRequest {
		t.Errorf("missing key: status = %d, want 400", w.Code)
	}
	// Invalid draft JSON -> 422.
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/diff?key=k", "not json"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid draft: status = %d, want 422", w.Code)
	}
}

func TestBackupDiff(t *testing.T) {
	e := newSourcesEnv(t)
	// Current state: web (2 instances) + vpc. Backup: web (1 instance) + s3.
	// From the restore perspective: s3 comes back (added), vpc is dropped
	// (removed), web's instance count differs (changed).
	current := `{"version":4,"terraform_version":"1.9.5","serial":9,"lineage":"lin-1","resources":[
		{"module":"","mode":"managed","type":"aws_instance","name":"web",
		 "provider":"provider[\"registry.terraform.io/hashicorp/aws\"]",
		 "instances":[{"index_key":0},{"index_key":1}]},
		{"module":"","mode":"managed","type":"aws_vpc","name":"main",
		 "provider":"provider[\"registry.terraform.io/hashicorp/aws\"]","instances":[{}]}
	]}`
	backupData := minState(7, "lin-1", "aws_instance.web", "aws_s3_bucket.logs")
	e.seed(t, "app.tfstate", current)

	e.expectBackupRow("b1", "s1", "app.tfstate", backupData, 7)
	e.expectSource("s1", e.dir)
	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/backups/b1/diff", "")
	if w.Code != http.StatusOK {
		t.Fatalf("diff: status = %d (%s)", w.Code, w.Body.String())
	}
	var diff struct {
		BackupSerial  *int64 `json:"backup_serial"`
		CurrentSerial *int64 `json:"current_serial"`
		Added         []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"added"`
		Removed []struct {
			Type string `json:"type"`
		} `json:"removed"`
		Changed []struct {
			Type      string `json:"type"`
			Instances int    `json:"instances"`
		} `json:"changed"`
		ApproximateChanged bool `json:"approximate_changed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &diff); err != nil {
		t.Fatalf("invalid diff json: %v (%s)", err, w.Body.String())
	}
	if diff.BackupSerial == nil || *diff.BackupSerial != 7 || diff.CurrentSerial == nil || *diff.CurrentSerial != 9 {
		t.Errorf("serials = %v/%v, want 7/9", diff.BackupSerial, diff.CurrentSerial)
	}
	if len(diff.Added) != 1 || diff.Added[0].Type != "aws_s3_bucket" {
		t.Errorf("added = %+v, want the backup-only aws_s3_bucket", diff.Added)
	}
	if len(diff.Removed) != 1 || diff.Removed[0].Type != "aws_vpc" {
		t.Errorf("removed = %+v, want the current-only aws_vpc", diff.Removed)
	}
	if len(diff.Changed) != 1 || diff.Changed[0].Type != "aws_instance" {
		t.Errorf("changed = %+v, want aws_instance (instance-count delta)", diff.Changed)
	}
	if !diff.ApproximateChanged {
		t.Error("approximate_changed must be true (instance-level heuristic)")
	}
}

func TestBackupDiff_CurrentMissing(t *testing.T) {
	e := newSourcesEnv(t)
	// No seeded file: the connector reports not-found, so restoring would
	// re-create everything - the whole backup lands in "added".
	backupData := minState(7, "lin-1", "aws_instance.web")
	e.expectBackupRow("b1", "s1", "ghost.tfstate", backupData, 7)
	e.expectSource("s1", e.dir)

	w := e.do(http.MethodGet, "/api/v1/sources/s1/state/backups/b1/diff", "")
	if w.Code != http.StatusOK {
		t.Fatalf("diff vs missing current: status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"current_serial":null`) {
		t.Errorf("current_serial must be null when the state object is gone: %s", body)
	}
	if !strings.Contains(body, "aws_instance") || !strings.Contains(body, `"removed":[]`) {
		t.Errorf("expected everything added, nothing removed: %s", body)
	}
}

func TestBackupDiff_Ownership(t *testing.T) {
	e := newSourcesEnv(t)
	backupData := minState(7, "lin-1", "aws_instance.web")
	e.expectBackupRow("b9", "OTHER", "app.tfstate", backupData, 7)
	e.expectSource("s1", e.dir)
	if w := e.do(http.MethodGet, "/api/v1/sources/s1/state/backups/b9/diff", ""); w.Code != http.StatusNotFound {
		t.Errorf("cross-source diff: status = %d, want 404", w.Code)
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
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").
		WithArgs(sqlmock.AnyArg(), "ghost").
		WillReturnRows(sqlmock.NewRows(apiSourceCols))
	if w := e.do(http.MethodPost, "/api/v1/sources/s1/state/backup?key=app.tfstate",
		`{"target_source_id":"ghost","target_key":"k"}`); w.Code != http.StatusNotFound {
		t.Errorf("missing target source: status = %d, want 404", w.Code)
	}
}

// GET /sources is bounded so the whole table can never be serialized in one
// response (#282). The default page is wide enough that no realistic install
// truncates, and `total` makes any truncation detectable rather than silent.
func TestListSourcesIsBounded(t *testing.T) {
	e := newSourcesEnv(t)
	cfg, _ := json.Marshal(map[string]any{"base_path": e.dir})
	row := func() *sqlmock.Rows {
		return sqlmock.NewRows(apiSourceCols).
			AddRow("s1", "demo", "local", "", cfg, []byte(`{}`), nil, "2026-06-10", "2026-06-10", testActingOrg)
	}

	// No params: capped at the 500 default, offset 0.
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").WithArgs(sqlmock.AnyArg(), 500, 0).WillReturnRows(row())
	e.mock.ExpectQuery("SELECT count.+WHERE organization_id").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	w := e.do(http.MethodGet, "/api/v1/sources", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"demo"`) {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	// The legacy `sources` key must survive — existing clients read it.
	if !strings.Contains(w.Body.String(), `"sources"`) || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Errorf("response must keep `sources` and add `total`: %s", w.Body.String())
	}

	// Explicit paging.
	e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").WithArgs(sqlmock.AnyArg(), 2, 4).WillReturnRows(row())
	e.mock.ExpectQuery("SELECT count.+WHERE organization_id").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(9))
	if w := e.do(http.MethodGet, "/api/v1/sources?page=3&per_page=2", ""); w.Code != http.StatusOK {
		t.Errorf("paged: status = %d (%s)", w.Code, w.Body.String())
	}

	// Over-cap and junk per_page fall back to the default rather than erroring,
	// so a hostile value cannot widen the response.
	for _, q := range []string{"?per_page=100000", "?per_page=-1", "?per_page=abc", "?page=0"} {
		e.mock.ExpectQuery("SELECT .+ FROM state_sources WHERE organization_id").WithArgs(sqlmock.AnyArg(), 500, 0).WillReturnRows(row())
		e.mock.ExpectQuery("SELECT count.+WHERE organization_id").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		if w := e.do(http.MethodGet, "/api/v1/sources"+q, ""); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d (%s)", q, w.Code, w.Body.String())
		}
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
