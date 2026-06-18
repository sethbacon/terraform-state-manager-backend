package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

func ciTemplateEnv(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewCITemplateHandlers(db, nil)
	repo := repositories.NewWorkflowTemplateRepository(db)

	r := gin.New()
	v1 := r.Group("/api/v1")
	v1.GET("/admin/ci/templates", h.ListCITemplates())
	v1.GET("/admin/ci/templates/:id", h.GetCITemplate())
	v1.POST("/admin/ci/templates", h.CreateCITemplate())
	v1.PUT("/admin/ci/templates/:id", h.UpdateCITemplate())
	v1.DELETE("/admin/ci/templates/:id", h.DeleteCITemplate())
	// Store-backed public serve (drift), to exercise override + fallback.
	v1.GET("/drift/workflow", serveWorkflowTemplate(repo, "drift"))
	return r, mock
}

func ciDo(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func wtAPIRow(builtin bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "provider", "kind", "profile", "name", "description", "content", "is_builtin", "created_at", "updated_at"}).
		AddRow("t1", "azure_devops", "drift", "default", "Azure Drift", "", "trigger: none", builtin, "2026-06-18", "2026-06-18")
}

func TestServeWorkflowTemplate_OverrideAndFallback(t *testing.T) {
	r, mock := ciTemplateEnv(t)

	// Override present in the store → its content is served.
	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE provider").
		WithArgs("azure_devops", "drift", "default").WillReturnRows(wtAPIRow(false))
	w := ciDo(r, http.MethodGet, "/api/v1/drift/workflow?provider=azure_devops", "")
	if w.Code != http.StatusOK || w.Body.String() != "trigger: none" {
		t.Fatalf("override: status=%d body=%q", w.Code, w.Body.String())
	}

	// No row → transparent fallback to the embedded const (contains callback_url).
	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE provider").
		WithArgs("azure_devops", "drift", "default").WillReturnError(sql.ErrNoRows)
	w = ciDo(r, http.MethodGet, "/api/v1/drift/workflow?provider=azure_devops", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "callback_url") {
		t.Fatalf("fallback: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestCITemplate_CreateValidation(t *testing.T) {
	r, _ := ciTemplateEnv(t)
	cases := []string{
		`{"provider":"jenkins","kind":"drift","profile":"p","name":"n","content":"c"}`,                // bad provider
		`{"provider":"azure_devops","kind":"plan","profile":"p","name":"n","content":"c"}`,            // bad kind
		`{"provider":"azure_devops","kind":"drift","profile":"bad profile","name":"n","content":"c"}`, // bad profile
		`{"provider":"azure_devops","kind":"drift","profile":"p","name":"","content":"c"}`,            // missing name
		`{"provider":"azure_devops","kind":"drift","profile":"p","name":"n","content":""}`,            // missing content
	}
	for _, body := range cases {
		if w := ciDo(r, http.MethodPost, "/api/v1/admin/ci/templates", body); w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for %s, got %d", body, w.Code)
		}
	}
}

func TestCITemplate_CreateConflictAndSuccess(t *testing.T) {
	r, mock := ciTemplateEnv(t)
	body := `{"provider":"azure_devops","kind":"drift","profile":"brunswick-azure","name":"BRN","description":"d","content":"yaml"}`

	// Conflict: an existing (provider,kind,profile) → 409, no insert.
	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE provider").
		WithArgs("azure_devops", "drift", "brunswick-azure").WillReturnRows(wtAPIRow(false))
	if w := ciDo(r, http.MethodPost, "/api/v1/admin/ci/templates", body); w.Code != http.StatusConflict {
		t.Fatalf("conflict: status=%d", w.Code)
	}

	// Success: no existing row, then insert returns the created row → 201.
	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE provider").
		WithArgs("azure_devops", "drift", "brunswick-azure").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("INSERT INTO workflow_templates").
		WithArgs("azure_devops", "drift", "brunswick-azure", "BRN", "d", "yaml", false).
		WillReturnRows(wtAPIRow(false))
	if w := ciDo(r, http.MethodPost, "/api/v1/admin/ci/templates", body); w.Code != http.StatusCreated {
		t.Fatalf("create: status=%d", w.Code)
	}
}

func TestCITemplate_ListGetUpdate(t *testing.T) {
	r, mock := ciTemplateEnv(t)

	mock.ExpectQuery("SELECT .+ FROM workflow_templates ORDER BY").WillReturnRows(wtAPIRow(true))
	if w := ciDo(r, http.MethodGet, "/api/v1/admin/ci/templates", ""); w.Code != http.StatusOK {
		t.Fatalf("list: status=%d", w.Code)
	}

	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE id").WithArgs("t1").WillReturnRows(wtAPIRow(true))
	if w := ciDo(r, http.MethodGet, "/api/v1/admin/ci/templates/t1", ""); w.Code != http.StatusOK {
		t.Fatalf("get: status=%d", w.Code)
	}

	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE id").WithArgs("ghost").WillReturnError(sql.ErrNoRows)
	if w := ciDo(r, http.MethodGet, "/api/v1/admin/ci/templates/ghost", ""); w.Code != http.StatusNotFound {
		t.Fatalf("get missing: status=%d", w.Code)
	}

	// Update success.
	mock.ExpectQuery("UPDATE workflow_templates SET").WithArgs("t1", "New", "nd", "new yaml").WillReturnRows(wtAPIRow(true))
	if w := ciDo(r, http.MethodPut, "/api/v1/admin/ci/templates/t1", `{"name":"New","description":"nd","content":"new yaml"}`); w.Code != http.StatusOK {
		t.Fatalf("update: status=%d", w.Code)
	}

	// Update missing → 404.
	mock.ExpectQuery("UPDATE workflow_templates SET").WithArgs("ghost", "n", "", "c").WillReturnError(sql.ErrNoRows)
	if w := ciDo(r, http.MethodPut, "/api/v1/admin/ci/templates/ghost", `{"name":"n","content":"c"}`); w.Code != http.StatusNotFound {
		t.Fatalf("update missing: status=%d", w.Code)
	}
}

func TestCITemplate_Delete(t *testing.T) {
	r, mock := ciTemplateEnv(t)

	// Built-in cannot be deleted → 403.
	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE id").WithArgs("t1").WillReturnRows(wtAPIRow(true))
	if w := ciDo(r, http.MethodDelete, "/api/v1/admin/ci/templates/t1", ""); w.Code != http.StatusForbidden {
		t.Fatalf("delete builtin: status=%d", w.Code)
	}

	// Non-builtin deletes → 204.
	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE id").WithArgs("t1").WillReturnRows(wtAPIRow(false))
	mock.ExpectExec("DELETE FROM workflow_templates WHERE id").WithArgs("t1").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := ciDo(r, http.MethodDelete, "/api/v1/admin/ci/templates/t1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d", w.Code)
	}

	// Missing → 404.
	mock.ExpectQuery("SELECT .+ FROM workflow_templates WHERE id").WithArgs("ghost").WillReturnError(sql.ErrNoRows)
	if w := ciDo(r, http.MethodDelete, "/api/v1/admin/ci/templates/ghost", ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete missing: status=%d", w.Code)
	}
}
