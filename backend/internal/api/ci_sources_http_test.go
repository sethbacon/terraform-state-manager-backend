package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
)

// newCISourcesEnv wires the CI-source handlers over sqlmock. Tests cover CRUD
// and every pre-network validation path; the provider proxy calls themselves
// (hardcoded ADO/GitHub bases) are covered by internal/pipelines' httptest
// suites, not exercised here.
func newCISourcesEnv(t *testing.T) *sourcesEnv {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	// newSQLMock, not sqlmock.New: it installs pgxparam.Converter, without which a
	// []string bound for `= ANY($n)` is not a valid driver.Value and the call
	// fails before any expectation is consulted. This rig had never passed one.
	db, mock, err := newSQLMock()
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
	// What middleware.TenantScope publishes in production. Stored directly rather
	// than resolved, so this rig needs no membership store — but it must be
	// stored, because a route that CREATES treats an unresolved scope as a 500
	// rather than as "no memberships" (#436).
	r.Use(func(c *gin.Context) {
		tenantscope.Store(c, tenantscope.Scope{OrgIDs: []string{testActingOrg}})
		c.Next()
	})
	v1 := r.Group("/api/v1/ci-sources")
	v1.GET("", h.ListCISources())
	v1.POST("", h.CreateCISource())
	v1.DELETE("/:id", h.DeleteCISource())
	v1.POST("/:id/verify", h.VerifyCISource())
	v1.GET("/:id/pipelines", h.ListSourcePipelines())
	v1.GET("/:id/repos", h.ListSourceRepos())
	v1.GET("/:id/repos/:repo/workflows", h.ListSourceWorkflows())
	v1.GET("/:id/service-connections", h.ListSourceServiceConnections())
	v1.POST("/:id/repos/:repo/pipelines", h.CreateSourcePipeline())
	v1.POST("/:id/repos/:repo/workflow-setup", h.SetupSourceWorkflow())
	v1.GET("/:id/repos/:repo/prs/:pr", h.GetSourcePRState())
	return &sourcesEnv{r: r, mock: mock}
}

var ciSrcCols = []string{"id", "name", "provider", "organization", "project", "auth_method", "encrypted_token", "tenant_id", "client_id", "encrypted_client_secret", "github_app_id", "github_installation_id", "encrypted_app_private_key", "created_at", "updated_at"}

func ciSrcRow(t *testing.T, provider string, project *string, token string) *sqlmock.Rows {
	t.Helper()
	enc, err := crypto.Encrypt([]byte(token))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return sqlmock.NewRows(ciSrcCols).
		AddRow("c1", "corp", provider, "corp-org", project, "pat", enc, nil, nil, nil, nil, nil, nil, "2026-06-10", "2026-06-10")
}

// appCiSrcRow builds an Entra app-auth ADO source row whose encrypted client
// secret decrypts to "the-secret".
func appCiSrcRow(t *testing.T) *sqlmock.Rows {
	t.Helper()
	enc, err := crypto.Encrypt([]byte("the-secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	proj := "Platform"
	return sqlmock.NewRows(ciSrcCols).
		AddRow("c1", "corp", "azure_devops", "corp-org", &proj, "app", nil, "the-tenant", "the-client", enc, nil, nil, nil, "2026-06-10", "2026-06-10")
}

func TestCISources_CreateApp(t *testing.T) {
	e := newCISourcesEnv(t)

	// Missing Entra fields.
	if w := e.do(http.MethodPost, "/api/v1/ci-sources",
		`{"name":"x","provider":"azure_devops","project":"P","organization":"o","auth_method":"app","client_id":"c","client_secret":"s"}`); w.Code != http.StatusBadRequest {
		t.Errorf("app missing tenant: status = %d, want 400", w.Code)
	}

	// Valid app source.
	e.mock.ExpectQuery("INSERT INTO ci_sources").WillReturnRows(appCiSrcRow(t))
	w := e.do(http.MethodPost, "/api/v1/ci-sources",
		`{"name":"corp","provider":"azure_devops","project":"Platform","organization":"corp-org","auth_method":"app","tenant_id":"the-tenant","client_id":"the-client","client_secret":"the-secret"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create app: status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "the-secret") {
		t.Error("create response leaked the client secret")
	}
	for _, want := range []string{`"auth_method":"app"`, `"has_client_secret":true`, `"tenant_id":"the-tenant"`, `"client_id":"the-client"`, `"has_token":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("create app body missing %s: %s", want, body)
		}
	}
}

func TestCISources_CreateGitHubApp(t *testing.T) {
	e := newCISourcesEnv(t)

	// Missing GitHub App fields.
	if w := e.do(http.MethodPost, "/api/v1/ci-sources",
		`{"name":"x","provider":"github_actions","organization":"o","auth_method":"app","github_app_id":"1"}`); w.Code != http.StatusBadRequest {
		t.Errorf("gh app missing fields: status = %d, want 400", w.Code)
	}
	// A non-PEM private key is rejected.
	if w := e.do(http.MethodPost, "/api/v1/ci-sources",
		`{"name":"x","provider":"github_actions","organization":"o","auth_method":"app","github_app_id":"1","github_installation_id":"2","app_private_key":"not-a-pem"}`); w.Code != http.StatusBadRequest {
		t.Errorf("gh app bad key: status = %d, want 400", w.Code)
	}

	// Valid GitHub App source.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	enc, err := crypto.Encrypt([]byte(pemStr))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	e.mock.ExpectQuery("INSERT INTO ci_sources").WillReturnRows(
		sqlmock.NewRows(ciSrcCols).
			AddRow("c1", "corp", "github_actions", "corp-org", nil, "app", nil, nil, nil, nil, "app-123", "inst-9", enc, "2026-06-10", "2026-06-10"))
	payload, _ := json.Marshal(map[string]string{
		"name": "corp-gh", "provider": "github_actions", "organization": "corp-org",
		"auth_method": "app", "github_app_id": "app-123", "github_installation_id": "inst-9",
		"app_private_key": pemStr,
	})
	w := e.do(http.MethodPost, "/api/v1/ci-sources", string(payload))
	if w.Code != http.StatusCreated {
		t.Fatalf("create gh app: status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The PEM (its "BEGIN ... PRIVATE KEY" marker) must never appear in the
	// response; only the has_app_private_key boolean is exposed.
	if strings.Contains(body, "BEGIN") || strings.Contains(body, "PRIVATE KEY") {
		t.Error("create response leaked the private key")
	}
	for _, want := range []string{`"auth_method":"app"`, `"has_app_private_key":true`, `"github_app_id":"app-123"`, `"github_installation_id":"inst-9"`, `"has_token":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("create gh app body missing %s: %s", want, body)
		}
	}
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

	e.mock.ExpectExec(`DELETE FROM ci_sources[\s\S]*organization_id`).WithArgs("c1", []string{testActingOrg}).
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
			AddRow("c1", "corp", "github_actions", "corp-org", nil, "pat", []byte("not-a-ciphertext"), nil, nil, nil, nil, nil, nil, "2026-06-10", "2026-06-10"))
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
