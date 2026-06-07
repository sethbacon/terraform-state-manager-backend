package ado

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testProject = "Contoso"
	testPAT     = "abc123pat"
)

// loadFixture reads a testdata JSON file or fails the test.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// mockServer holds the httptest server plus knobs to assert auth and inject
// per-resource-type failures for the resilient-enumeration test.
type mockServer struct {
	failPaths     map[string]int // path substring -> status code to return
	requirePAT    bool
	seenAuthValue string // last Authorization header observed (for assertion)
}

// newMockADO builds an httptest.Server that mimics the Azure DevOps REST API
// 7.1 surface used by this package. It serves the testdata fixtures, drives
// pipelines continuation-token paging across two pages, and optionally injects
// failures for specific resource paths.
func newMockADO(t *testing.T, m *mockServer) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	handleEnvelope := func(fixture string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			m.seenAuthValue = r.Header.Get("Authorization")
			if status, fail := matchFailure(m, r.URL.Path); fail {
				http.Error(w, "injected failure", status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(loadFixture(t, fixture))
		}
	}

	// Repositories
	mux.HandleFunc("/"+testProject+"/_apis/git/repositories", handleEnvelope("repositories.json"))

	// Pipelines — two-page continuation-token paging.
	mux.HandleFunc("/"+testProject+"/_apis/pipelines", func(w http.ResponseWriter, r *http.Request) {
		m.seenAuthValue = r.Header.Get("Authorization")
		if status, fail := matchFailure(m, r.URL.Path); fail {
			http.Error(w, "injected failure", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("continuationToken") == "" {
			// First page: advertise a continuation token for page 2.
			w.Header().Set(continuationTokenHeader, "page2token")
			_, _ = w.Write(loadFixture(t, "pipelines_page1.json"))
			return
		}
		// Second page: no continuation token -> paging stops.
		_, _ = w.Write(loadFixture(t, "pipelines_page2.json"))
	})

	// Branch policies
	mux.HandleFunc("/"+testProject+"/_apis/policy/configurations", handleEnvelope("policies.json"))

	// Variable groups
	mux.HandleFunc("/"+testProject+"/_apis/distributedtask/variablegroups", handleEnvelope("variablegroups.json"))

	// Service connections
	mux.HandleFunc("/"+testProject+"/_apis/serviceendpoint/endpoints", handleEnvelope("serviceconnections.json"))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func matchFailure(m *mockServer, path string) (int, bool) {
	for sub, status := range m.failPaths {
		if strings.Contains(path, sub) {
			return status, true
		}
	}
	return 0, false
}

func newTestClient(t *testing.T, baseURL, token string) *Client {
	t.Helper()
	c, err := NewClient(Config{
		OrganizationURL: baseURL,
		Project:         testProject,
		Token:           token,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestNewClient_Validation(t *testing.T) {
	if _, err := NewClient(Config{Project: "P"}); err == nil {
		t.Error("expected error when OrganizationURL is empty")
	}
	if _, err := NewClient(Config{OrganizationURL: "https://x"}); err == nil {
		t.Error("expected error when Project is empty")
	}
	// Token may be empty.
	if _, err := NewClient(Config{OrganizationURL: "https://x", Project: "P"}); err != nil {
		t.Errorf("unexpected error with empty token: %v", err)
	}
}

// TestPATAuthHeader asserts the personal access token is sent as exactly
// Authorization: Basic base64(":" + token).
func TestPATAuthHeader(t *testing.T) {
	m := &mockServer{}
	srv := newMockADO(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	if _, err := c.ListRepositories(context.Background()); err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+testPAT))
	if m.seenAuthValue != want {
		t.Errorf("Authorization header = %q, want %q", m.seenAuthValue, want)
	}
}

// TestNoAuthHeaderWhenTokenEmpty verifies unauthenticated enumeration sends no
// Authorization header (so mock-based tests run without credentials).
func TestNoAuthHeaderWhenTokenEmpty(t *testing.T) {
	m := &mockServer{}
	srv := newMockADO(t, m)
	c := newTestClient(t, srv.URL, "")

	if _, err := c.ListRepositories(context.Background()); err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if m.seenAuthValue != "" {
		t.Errorf("expected no Authorization header, got %q", m.seenAuthValue)
	}
}

func TestListRepositories(t *testing.T) {
	srv := newMockADO(t, &mockServer{})
	c := newTestClient(t, srv.URL, testPAT)

	repos, err := c.ListRepositories(context.Background())
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repositories, want 2", len(repos))
	}
	if repos[0].Name != "platform-infra" {
		t.Errorf("repos[0].Name = %q, want platform-infra", repos[0].Name)
	}
	if repos[0].DefaultBranch != "refs/heads/main" {
		t.Errorf("repos[0].DefaultBranch = %q, want refs/heads/main", repos[0].DefaultBranch)
	}
	if repos[0].SizeBytes != 1048576 {
		t.Errorf("repos[0].SizeBytes = %d, want 1048576", repos[0].SizeBytes)
	}
	if repos[0].RemoteURL == "" {
		t.Error("repos[0].RemoteURL is empty")
	}
}

// TestListPipelines_Pagination exercises continuation-token paging across two
// pages and verifies the combined result.
func TestListPipelines_Pagination(t *testing.T) {
	srv := newMockADO(t, &mockServer{})
	c := newTestClient(t, srv.URL, testPAT)

	pipelines, err := c.ListPipelines(context.Background())
	if err != nil {
		t.Fatalf("ListPipelines: %v", err)
	}
	if len(pipelines) != 3 {
		t.Fatalf("got %d pipelines across pages, want 3", len(pipelines))
	}
	wantNames := map[string]bool{
		"platform-infra-CI":    false,
		"app-services-CI":      false,
		"nightly-drift-detect": false,
	}
	for _, p := range pipelines {
		if _, ok := wantNames[p.Name]; ok {
			wantNames[p.Name] = true
		}
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("expected pipeline %q across both pages, not found", name)
		}
	}
}

func TestListBranchPolicies(t *testing.T) {
	srv := newMockADO(t, &mockServer{})
	c := newTestClient(t, srv.URL, testPAT)

	policies, err := c.ListBranchPolicies(context.Background())
	if err != nil {
		t.Fatalf("ListBranchPolicies: %v", err)
	}
	if len(policies) != 2 {
		t.Fatalf("got %d policies, want 2", len(policies))
	}
	if policies[0].DisplayName != "Minimum number of reviewers" {
		t.Errorf("policies[0].DisplayName = %q", policies[0].DisplayName)
	}
	if !policies[0].IsEnabled {
		t.Error("policies[0].IsEnabled = false, want true")
	}
	if policies[1].IsEnabled {
		t.Error("policies[1].IsEnabled = true, want false")
	}
	if policies[0].ID != 11 {
		t.Errorf("policies[0].ID = %d, want 11", policies[0].ID)
	}
}

// TestListVariableGroups_NamesOnly verifies only variable names are captured and
// no secret values are present anywhere in the returned data.
func TestListVariableGroups_NamesOnly(t *testing.T) {
	srv := newMockADO(t, &mockServer{})
	c := newTestClient(t, srv.URL, testPAT)

	groups, err := c.ListVariableGroups(context.Background())
	if err != nil {
		t.Fatalf("ListVariableGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d variable groups, want 2", len(groups))
	}

	first := groups[0]
	if first.Name != "platform-shared" {
		t.Errorf("groups[0].Name = %q, want platform-shared", first.Name)
	}
	// Names are sorted deterministically.
	wantNames := []string{"ARM_CLIENT_SECRET", "TF_BACKEND_RESOURCE_GROUP", "TF_BACKEND_STORAGE_ACCOUNT"}
	if len(first.VariableNames) != len(wantNames) {
		t.Fatalf("got %d variable names, want %d", len(first.VariableNames), len(wantNames))
	}
	for i, n := range wantNames {
		if first.VariableNames[i] != n {
			t.Errorf("VariableNames[%d] = %q, want %q", i, first.VariableNames[i], n)
		}
	}

	// Confirm a known secret value never leaks into the captured structure.
	for _, g := range groups {
		for _, n := range g.VariableNames {
			if strings.Contains(n, "rg-tfstate-prod") {
				t.Errorf("variable name %q contains a value, not just a name", n)
			}
		}
	}
}

func TestListServiceConnections(t *testing.T) {
	srv := newMockADO(t, &mockServer{})
	c := newTestClient(t, srv.URL, testPAT)

	conns, err := c.ListServiceConnections(context.Background())
	if err != nil {
		t.Fatalf("ListServiceConnections: %v", err)
	}
	if len(conns) != 2 {
		t.Fatalf("got %d service connections, want 2", len(conns))
	}
	if conns[0].Name != "azure-prod-connection" {
		t.Errorf("conns[0].Name = %q", conns[0].Name)
	}
	if conns[0].Type != "azurerm" {
		t.Errorf("conns[0].Type = %q, want azurerm", conns[0].Type)
	}
}

// TestEnumerateMigrationPlan_Success verifies a fully populated plan with all
// five lists and correct counts.
func TestEnumerateMigrationPlan_Success(t *testing.T) {
	srv := newMockADO(t, &mockServer{})
	c := newTestClient(t, srv.URL, testPAT)

	plan, err := EnumerateMigrationPlan(context.Background(), c)
	if err != nil {
		t.Fatalf("EnumerateMigrationPlan: %v", err)
	}
	if plan == nil {
		t.Fatal("plan is nil")
	}

	checks := []struct {
		name string
		got  int
		want int
	}{
		{"RepositoryCount", plan.RepositoryCount, 2},
		{"PipelineCount", plan.PipelineCount, 3},
		{"BranchPolicyCount", plan.BranchPolicyCount, 2},
		{"VariableGroupCount", plan.VariableGroupCount, 2},
		{"ServiceConnectionCount", plan.ServiceConnectionCount, 2},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %d, want %d", ck.name, ck.got, ck.want)
		}
	}

	if len(plan.Repositories) != 2 {
		t.Errorf("Repositories length = %d, want 2", len(plan.Repositories))
	}
	if len(plan.Pipelines) != 3 {
		t.Errorf("Pipelines length = %d, want 3", len(plan.Pipelines))
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("expected no warnings on success, got %v", plan.Warnings)
	}
	if plan.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is zero")
	}
}

// TestEnumerateMigrationPlan_Resilient verifies that a 403 on one resource type
// and a 404 on another produce Warnings while the remaining three lists are
// still returned. Both status codes are non-retryable, so the failed calls
// surface immediately (the shared HTTP client's retry-on-5xx behaviour is
// covered by that package's own tests, not exercised here to keep this fast).
func TestEnumerateMigrationPlan_Resilient(t *testing.T) {
	m := &mockServer{
		failPaths: map[string]int{
			"/_apis/policy/configurations":          http.StatusForbidden, // 403
			"/_apis/distributedtask/variablegroups": http.StatusNotFound,  // 404
		},
	}
	srv := newMockADO(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	plan, err := EnumerateMigrationPlan(context.Background(), c)
	if err != nil {
		t.Fatalf("EnumerateMigrationPlan returned fatal error, want resilient: %v", err)
	}

	// Two failures -> two warnings.
	if len(plan.Warnings) != 2 {
		t.Fatalf("got %d warnings, want 2: %v", len(plan.Warnings), plan.Warnings)
	}
	joined := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(joined, "branch_policies") {
		t.Errorf("warnings missing branch_policies entry: %v", plan.Warnings)
	}
	if !strings.Contains(joined, "variable_groups") {
		t.Errorf("warnings missing variable_groups entry: %v", plan.Warnings)
	}

	// The failed types are empty.
	if plan.BranchPolicyCount != 0 {
		t.Errorf("BranchPolicyCount = %d, want 0 after failure", plan.BranchPolicyCount)
	}
	if plan.VariableGroupCount != 0 {
		t.Errorf("VariableGroupCount = %d, want 0 after failure", plan.VariableGroupCount)
	}

	// The other three types are still fully enumerated.
	if plan.RepositoryCount != 2 {
		t.Errorf("RepositoryCount = %d, want 2", plan.RepositoryCount)
	}
	if plan.PipelineCount != 3 {
		t.Errorf("PipelineCount = %d, want 3", plan.PipelineCount)
	}
	if plan.ServiceConnectionCount != 2 {
		t.Errorf("ServiceConnectionCount = %d, want 2", plan.ServiceConnectionCount)
	}
}

// TestDefaultAPIVersion verifies every request carries api-version=7.1.
func TestDefaultAPIVersion(t *testing.T) {
	var seenVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenVersion = r.URL.Query().Get("api-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"value":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, testPAT)
	if _, err := c.ListRepositories(context.Background()); err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if seenVersion != defaultAPIVersion {
		t.Errorf("api-version = %q, want %q", seenVersion, defaultAPIVersion)
	}
}
