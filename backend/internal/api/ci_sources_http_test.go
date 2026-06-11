package api

import (
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
)

// newCISourcesEnv wires the CI-source handlers over sqlmock. Tests cover CRUD
// and every pre-network validation path; the provider proxy calls themselves
// (hardcoded ADO/GitHub bases) are covered by internal/pipelines' httptest
// suites, not exercised here.
func newCISourcesEnv(t *testing.T) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Unlike the auditor-wrapped handlers, CISourceHandlers writes audits via
	// the raw identity repo, which requires a non-nil DB. Audit INSERTs are
	// best-effort, so this mock needs no expectations.
	idDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New (identity): %v", err)
	}
	t.Cleanup(func() { idDB.Close() })

	h := NewCISourceHandlers(db, idDB)
	r := gin.New()
	v1 := r.Group("/api/v1/ci-sources")
	v1.GET("", h.ListCISources())
	v1.POST("", h.CreateCISource())
	v1.DELETE("/:id", h.DeleteCISource())
	v1.GET("/:id/pipelines", h.ListSourcePipelines())
	v1.GET("/:id/repos", h.ListSourceRepos())
	v1.GET("/:id/repos/:repo/workflows", h.ListSourceWorkflows())
	v1.GET("/:id/service-connections", h.ListSourceServiceConnections())
	v1.POST("/:id/repos/:repo/pipelines", h.CreateSourcePipeline())
	v1.POST("/:id/repos/:repo/workflow-setup", h.SetupSourceWorkflow())
	v1.GET("/:id/repos/:repo/prs/:pr", h.GetSourcePRState())
	return &sourcesEnv{r: r, mock: mock}
}

var ciSrcCols = []string{"id", "name", "provider", "organization", "project", "encrypted_token", "created_at", "updated_at"}

func ciSrcRow(t *testing.T, provider string, project *string, token string) *sqlmock.Rows {
	t.Helper()
	enc, err := crypto.Encrypt([]byte(token))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return sqlmock.NewRows(ciSrcCols).
		AddRow("c1", "corp", provider, "corp-org", project, enc, "2026-06-10", "2026-06-10")
}

func TestCISources_CRUD(t *testing.T) {
	e := newCISourcesEnv(t)
	proj := "Platform"

	e.mock.ExpectQuery("SELECT .+ FROM ci_sources ORDER BY name").
		WillReturnRows(ciSrcRow(t, "azure_devops", &proj, "pat"))
	w := e.do(http.MethodGet, "/api/v1/ci-sources", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "pat") && !strings.Contains(w.Body.String(), `"corp"`) {
		t.Error("list response shape wrong")
	}
	if strings.Contains(w.Body.String(), "encrypted_token") {
		t.Error("list leaked the encrypted token field")
	}

	// Validation matrix.
	for body, why := range map[string]string{
		`{`: "invalid JSON",
		`{"name":"x","provider":"github_actions","organization":"o"}`:              "missing token",
		`{"name":"x","provider":"jenkins","organization":"o","token":"t"}`:         "unsupported provider",
		`{"name":"x","provider":"azure_devops","organization":"o","token":"t"}`:    "ADO without project",
		`{"name":" ","provider":"github_actions","organization":"o","token":"t"}`:  "blank name",
		`{"name":"x","provider":"github_actions","organization":"  ","token":"t"}`: "blank org",
	} {
		if w := e.do(http.MethodPost, "/api/v1/ci-sources", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", why, w.Code)
		}
	}

	e.mock.ExpectQuery("INSERT INTO ci_sources").
		WillReturnRows(ciSrcRow(t, "github_actions", nil, "ghp"))
	w = e.do(http.MethodPost, "/api/v1/ci-sources",
		`{"name":"corp","provider":"github_actions","organization":"corp-org","token":"ghp_secret"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ghp_secret") {
		t.Error("create response leaked the token")
	}

	e.mock.ExpectExec("DELETE FROM ci_sources").WithArgs("c1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/ci-sources/c1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w.Code)
	}
}

func TestCISources_LoadWithTokenGuards(t *testing.T) {
	e := newCISourcesEnv(t)

	// Missing source → 404 (any discovery route exercises loadWithToken).
	e.mock.ExpectQuery("SELECT .+ FROM ci_sources WHERE id").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(ciSrcCols))
	if w := e.do(http.MethodGet, "/api/v1/ci-sources/ghost/repos", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing source: status = %d, want 404", w.Code)
	}

	// Corrupted sealed token → 500 before any provider call.
	e.mock.ExpectQuery("SELECT .+ FROM ci_sources WHERE id").WithArgs("c1").
		WillReturnRows(sqlmock.NewRows(ciSrcCols).
			AddRow("c1", "corp", "github_actions", "corp-org", nil, []byte("not-a-ciphertext"), "2026-06-10", "2026-06-10"))
	if w := e.do(http.MethodGet, "/api/v1/ci-sources/c1/repos", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("corrupt token: status = %d, want 500", w.Code)
	}
}

func TestCreateSourcePipeline_Validation(t *testing.T) {
	e := newCISourcesEnv(t)

	// GitHub sources cannot create ADO pipeline definitions.
	e.mock.ExpectQuery("SELECT .+ FROM ci_sources WHERE id").WithArgs("c1").
		WillReturnRows(ciSrcRow(t, "github_actions", nil, "ghp"))
	w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/repos/r1/pipelines", `{"name":"TSM Drift"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("github source: status = %d, want 400", w.Code)
	}

	// ADO source with a blank name.
	proj := "Platform"
	e.mock.ExpectQuery("SELECT .+ FROM ci_sources WHERE id").WithArgs("c1").
		WillReturnRows(ciSrcRow(t, "azure_devops", &proj, "pat"))
	w = e.do(http.MethodPost, "/api/v1/ci-sources/c1/repos/r1/pipelines", `{"name":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("blank name: status = %d, want 400", w.Code)
	}
}

func TestSetupSourceWorkflow_Validation(t *testing.T) {
	e := newCISourcesEnv(t)
	proj := "Platform"

	expectSrc := func() {
		e.mock.ExpectQuery("SELECT .+ FROM ci_sources WHERE id").WithArgs("c1").
			WillReturnRows(ciSrcRow(t, "azure_devops", &proj, "pat"))
	}

	expectSrc()
	if w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/repos/r1/workflow-setup", `{`); w.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: status = %d, want 400", w.Code)
	}

	expectSrc()
	if w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/repos/r1/workflow-setup", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("no content: status = %d, want 400", w.Code)
	}

	expectSrc()
	if w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/repos/r1/workflow-setup",
		`{"files":[{"kind":"malware","content":"x"}]}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown kind: status = %d, want 400", w.Code)
	}

	expectSrc()
	huge := strings.Repeat("y", 64*1024+1)
	if w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/repos/r1/workflow-setup",
		`{"files":[{"kind":"drift","content":"`+huge+`"}]}`); w.Code != http.StatusBadRequest {
		t.Errorf("oversize content: status = %d, want 400", w.Code)
	}
}

func TestGetSourcePRState_Validation(t *testing.T) {
	e := newCISourcesEnv(t)
	proj := "Platform"

	for _, pr := range []string{"abc", "0", "-3"} {
		e.mock.ExpectQuery("SELECT .+ FROM ci_sources WHERE id").WithArgs("c1").
			WillReturnRows(ciSrcRow(t, "azure_devops", &proj, "pat"))
		if w := e.do(http.MethodGet, "/api/v1/ci-sources/c1/repos/r1/prs/"+pr, ""); w.Code != http.StatusBadRequest {
			t.Errorf("pr=%s: status = %d, want 400", pr, w.Code)
		}
	}
}
