package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/pipelines"
)

// fakeWorkloadIdentityCredential is a test double for
// pipelines.ADOTokenCredential, standing in for a real AKS federated identity
// in TestVerifyCISource_WorkloadIdentity.
type fakeWorkloadIdentityCredential struct{ token string }

func (f fakeWorkloadIdentityCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: f.token}, nil
}

// fakeADO serves the minimal Azure DevOps surface the discovery/setup handlers
// proxy to. The pipelines package's own tests cover the protocol details; these
// tests cover the handler glue end to end.
func fakeADO(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_apis/pipelines") && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"count":1,"value":[{"id":7,"name":"TSM Drift","folder":"\\"}]}`)
		case strings.HasSuffix(r.URL.Path, "/_apis/pipelines") && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":42,"name":"TSM Drift","folder":"\\"}`)
		case strings.HasSuffix(r.URL.Path, "/_apis/git/repositories"):
			fmt.Fprint(w, `{"value":[{"id":"r1","name":"infra","defaultBranch":"refs/heads/main"}]}`)
		case strings.Contains(r.URL.Path, "/_apis/serviceendpoint/endpoints"):
			fmt.Fprint(w, `{"value":[{"id":"sc1","name":"azure-prod","type":"azurerm"}]}`)
		case strings.Contains(r.URL.Path, "/pullrequests/"):
			fmt.Fprint(w, `{"pullRequestId":12,"status":"completed"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestVerifyCISource_App: VerifyCISource on an app source mints an Entra token
// and presents it to ADO as a Bearer credential, end to end.
func TestVerifyCISource_App(t *testing.T) {
	pipelines.ResetEntraTokenCacheForTest()

	entraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"access_token":"minted","expires_in":3600}`)
	}))
	t.Cleanup(entraSrv.Close)
	defer pipelines.OverrideEntraLoginURLForTest(entraSrv.URL)()

	var gotAuth string
	adoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"count":0,"value":[]}`)
	}))
	t.Cleanup(adoSrv.Close)
	defer pipelines.OverrideBaseURLsForTest(adoSrv.URL, "")()

	e := newCISourcesEnv(t)
	e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
		WillReturnRows(appCiSrcRow(t))
	w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/verify", "")
	if w.Code != http.StatusOK {
		t.Fatalf("verify app: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("verify app body = %s", w.Body.String())
	}
	if gotAuth != "Bearer minted" {
		t.Errorf("ADO auth = %q, want Bearer minted (app token)", gotAuth)
	}
}

// TestVerifyCISource_WorkloadIdentity: VerifyCISource on a workload_identity
// source mints via AKS Workload Identity's federated-token exchange (no
// decryption at all -- there is no secret column to decrypt) and presents it
// to ADO as a Bearer credential, end to end.
func TestVerifyCISource_WorkloadIdentity(t *testing.T) {
	restoreFactory := pipelines.OverrideWorkloadIdentityCredentialFactoryForTest(
		func(clientID string) (pipelines.ADOTokenCredential, error) {
			return fakeWorkloadIdentityCredential{token: "wi-minted"}, nil
		})
	defer restoreFactory()

	var gotAuth string
	adoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"count":0,"value":[]}`)
	}))
	t.Cleanup(adoSrv.Close)
	defer pipelines.OverrideBaseURLsForTest(adoSrv.URL, "")()

	e := newCISourcesEnv(t)
	e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
		WillReturnRows(wiCiSrcRow(t, "the-wi-client"))
	w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/verify", "")
	if w.Code != http.StatusOK {
		t.Fatalf("verify workload_identity: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("verify workload_identity body = %s", w.Body.String())
	}
	if gotAuth != "Bearer wi-minted" {
		t.Errorf("ADO auth = %q, want Bearer wi-minted (workload identity token)", gotAuth)
	}
}

// TestUpdateCISource_EvictsCachedADOToken is the second high-risk proof the
// spec names directly: a token minted under the credential a PUT is
// REPLACING must never be served again. Proving it against a request that
// resends the SAME tenant/client/secret triple is what isolates "the route
// evicts unconditionally" from "a different credential value happened to
// hash to a different cache key" -- the latter would pass even if the
// eviction call in UpdateCISource were deleted entirely.
func TestUpdateCISource_EvictsCachedADOToken(t *testing.T) {
	pipelines.ResetEntraTokenCacheForTest()

	var mintCalls int32
	entraSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&mintCalls, 1)
		fmt.Fprint(w, `{"access_token":"minted","expires_in":3600}`)
	}))
	t.Cleanup(entraSrv.Close)
	defer pipelines.OverrideEntraLoginURLForTest(entraSrv.URL)()

	adoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"count":0,"value":[]}`)
	}))
	t.Cleanup(adoSrv.Close)
	defer pipelines.OverrideBaseURLsForTest(adoSrv.URL, "")()

	e := newCISourcesEnv(t)

	// 1) Verify mints and caches a token for the source's current credential.
	e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
		WillReturnRows(appCiSrcRow(t))
	if w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/verify", ""); w.Code != http.StatusOK {
		t.Fatalf("verify (priming): status = %d (%s)", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&mintCalls); got != 1 {
		t.Fatalf("mint calls = %d after priming, want 1", got)
	}

	// 2) PUT re-sends the EXACT SAME tenant/client/secret -- a no-op rotation
	// in terms of credential VALUE. If the route only evicted on a changed
	// value, the primed cache entry would still be sitting there afterward.
	e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
		WillReturnRows(appCiSrcRow(t))
	e.mock.ExpectQuery("UPDATE ci_sources").WillReturnRows(appCiSrcRow(t))
	e.idMock.ExpectQuery("INSERT INTO audit_logs").WillReturnRows(auditInsertReturn())
	w := e.do(http.MethodPut, "/api/v1/ci-sources/c1",
		`{"name":"corp","provider":"azure_devops","project":"Platform","organization":"corp-org","auth_method":"app","tenant_id":"the-tenant","client_id":"the-client","client_secret":"the-secret"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: status = %d (%s)", w.Code, w.Body.String())
	}

	// 3) A subsequent Verify must mint AGAIN: the primed entry must be gone,
	// not silently reused because the credential happens to be unchanged.
	e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
		WillReturnRows(appCiSrcRow(t))
	if w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/verify", ""); w.Code != http.StatusOK {
		t.Fatalf("verify (post-update): status = %d (%s)", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&mintCalls); got != 2 {
		t.Fatalf("mint calls = %d after the update, want 2: the update must evict the cached token "+
			"rather than let a token minted under the credential being replaced be served again", got)
	}
}

// ghAppCiSrcRow builds a GitHub App source row whose encrypted private key
// decrypts to a freshly generated RSA PEM (so minting can sign a real JWT).
func ghAppCiSrcRow(t *testing.T) *sqlmock.Rows {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	enc, err := crypto.Encrypt(pemBytes)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return sqlmock.NewRows(ciSrcCols).
		AddRow("c1", "corp", "github_actions", "corp-org", nil, "app", nil, nil, nil, nil, "app-123", "inst-9", enc, "2026-06-10", "2026-06-10", testActingOrg)
}

// TestVerifyCISource_GitHubApp: VerifyCISource on a GitHub App source mints an
// installation token (signing an app JWT) and verifies via the installation
// repositories endpoint, end to end.
func TestVerifyCISource_GitHubApp(t *testing.T) {
	pipelines.ResetGitHubAppTokenCacheForTest()

	var sawTokenMint, sawVerify bool
	var verifyAuth string
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			sawTokenMint = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"ghs_inst","expires_at":"2099-01-01T00:00:00Z"}`)
		case strings.Contains(r.URL.Path, "/installation/repositories"):
			sawVerify = true
			verifyAuth = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"total_count":0,"repositories":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ghSrv.Close)
	defer pipelines.OverrideBaseURLsForTest("", ghSrv.URL)()

	e := newCISourcesEnv(t)
	e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
		WillReturnRows(ghAppCiSrcRow(t))
	w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/verify", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("verify gh app: status = %d (%s)", w.Code, w.Body.String())
	}
	if !sawTokenMint || !sawVerify {
		t.Errorf("expected mint(%v) and verify(%v) calls", sawTokenMint, sawVerify)
	}
	if verifyAuth != "Bearer ghs_inst" {
		t.Errorf("verify auth = %q, want Bearer ghs_inst (installation token)", verifyAuth)
	}
}

func TestCISourceProxies_AzureDevOps(t *testing.T) {
	srv := fakeADO(t)
	restore := pipelines.OverrideBaseURLsForTest(srv.URL, "")
	defer restore()

	e := newCISourcesEnv(t)
	proj := "Platform"
	expectSrc := func() {
		e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
			WillReturnRows(ciSrcRow(t, "azure_devops", &proj, "pat"))
	}

	expectSrc()
	w := e.do(http.MethodGet, "/api/v1/ci-sources/c1/pipelines", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "TSM Drift") {
		t.Fatalf("pipelines: status = %d (%s)", w.Code, w.Body.String())
	}

	expectSrc()
	w = e.do(http.MethodGet, "/api/v1/ci-sources/c1/repos", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"infra"`) {
		t.Fatalf("repos: status = %d (%s)", w.Code, w.Body.String())
	}

	expectSrc()
	w = e.do(http.MethodGet, "/api/v1/ci-sources/c1/service-connections", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "azure-prod") {
		t.Fatalf("service connections: status = %d (%s)", w.Code, w.Body.String())
	}

	expectSrc()
	w = e.do(http.MethodPost, "/api/v1/ci-sources/c1/repos/r1/pipelines", `{"name":"TSM Drift"}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), "42") {
		t.Fatalf("create pipeline: status = %d (%s)", w.Code, w.Body.String())
	}

	expectSrc()
	w = e.do(http.MethodGet, "/api/v1/ci-sources/c1/repos/r1/prs/12", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "merged") {
		t.Fatalf("pr state: status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestCISourceProxies_GitHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/repos"):
			fmt.Fprint(w, `[{"id":1,"name":"infra","full_name":"corp-org/infra","default_branch":"main"}]`)
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			fmt.Fprint(w, `{"workflows":[{"id":1,"name":"Drift","path":".github/workflows/tsm-drift.yml","state":"active"}]}`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			fmt.Fprint(w, `{"number":5,"state":"open","merged":false}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	restore := pipelines.OverrideBaseURLsForTest("", srv.URL)
	defer restore()

	e := newCISourcesEnv(t)
	expectSrc := func() {
		e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
			WillReturnRows(ciSrcRow(t, "github_actions", nil, "ghp"))
	}

	expectSrc()
	w := e.do(http.MethodGet, "/api/v1/ci-sources/c1/repos", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"infra"`) {
		t.Fatalf("repos: status = %d (%s)", w.Code, w.Body.String())
	}

	expectSrc()
	w = e.do(http.MethodGet, "/api/v1/ci-sources/c1/repos/infra/workflows", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "tsm-drift.yml") {
		t.Fatalf("workflows: status = %d (%s)", w.Code, w.Body.String())
	}

	expectSrc()
	w = e.do(http.MethodGet, "/api/v1/ci-sources/c1/repos/infra/prs/5", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "open") {
		t.Fatalf("pr state: status = %d (%s)", w.Code, w.Body.String())
	}

	// GitHub sources don't expose ADO-only discovery.
	expectSrc()
	if w := e.do(http.MethodGet, "/api/v1/ci-sources/c1/pipelines", ""); w.Code != http.StatusBadRequest {
		t.Errorf("github pipelines listing: status = %d, want 400", w.Code)
	}
}

func TestDriftDispatch_HappyPathOverFakeProvider(t *testing.T) {
	var dispatched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/runs") {
			dispatched = true
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	restore := pipelines.OverrideBaseURLsForTest(srv.URL, "")
	defer restore()

	e := newDriftEnv(t)
	e.mock.ExpectQuery("FROM pipeline_connections WHERE organization_id = ANY").
		WithArgs([]string{testActingOrg}, "p1").
		WillReturnRows(pipelineHTTPRow(t, "azure_devops", "pat",
			map[string]any{"organization": "corp", "project": "Platform", "pipeline_id": "7"}))
	e.mock.ExpectQuery("INSERT INTO drift_runs").WillReturnRows(driftRow("tok-1"))

	w := e.do(http.MethodPost, "/api/v1/drift/runs", `{"pipeline_connection_id":"p1","state_key":"app.tfstate"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("dispatch: status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if !dispatched {
		t.Error("provider never received the run dispatch")
	}
	if strings.Contains(w.Body.String(), "tok-1") {
		t.Error("accepted run leaked the callback token")
	}
}

func TestSetupSourceWorkflow_ProviderFailureIs502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"repo not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	restore := pipelines.OverrideBaseURLsForTest(srv.URL, srv.URL)
	defer restore()

	e := newCISourcesEnv(t)
	proj := "Platform"
	e.mock.ExpectQuery("FROM ci_sources WHERE organization_id = ANY").WithArgs([]string{testActingOrg}, "c1").
		WillReturnRows(ciSrcRow(t, "azure_devops", &proj, "pat"))

	w := e.do(http.MethodPost, "/api/v1/ci-sources/c1/repos/ghost/workflow-setup",
		`{"files":[{"kind":"drift","content":"yaml"}]}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("provider failure: status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
}
