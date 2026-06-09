package ado

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// writeMock is an httptest-backed mock of the Azure DevOps write surface. For
// each registered path it asserts the request is a POST carrying a JSON body,
// captures the decoded body for assertions, and serves a "created" fixture.
// Per-path status overrides inject 409 (already-exists) and 5xx (error) cases.
type writeMock struct {
	t            *testing.T
	statusByPath map[string]int            // path substring -> status to return instead of 200
	lastBody     map[string]map[string]any // path substring -> last decoded request body
	lastMethod   string
	lastContent  string
}

func newWriteServer(t *testing.T, m *writeMock) *httptest.Server {
	t.Helper()
	m.t = t
	m.lastBody = map[string]map[string]any{}

	routes := map[string]string{
		"/_apis/git/repositories":               "repository_created.json",
		"/_apis/pipelines":                      "pipeline_created.json",
		"/_apis/policy/configurations":          "policy_created.json",
		"/_apis/distributedtask/variablegroups": "variablegroup_created.json",
		"/_apis/serviceendpoint/endpoints":      "serviceconnection_created.json",
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.lastMethod = r.Method
		m.lastContent = r.Header.Get("Content-Type")

		// Decode and retain the request body for assertions.
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		for sub, fixture := range routes {
			if strings.Contains(r.URL.Path, sub) {
				m.lastBody[sub] = decoded
				if status, ok := m.statusByPath[sub]; ok {
					http.Error(w, `{"message":"injected"}`, status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(loadFixture(t, fixture))
				return
			}
		}
		http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestCreateRepository_Success verifies a created repository is returned and the
// POST body carries the repository name plus the target project reference.
func TestCreateRepository_Success(t *testing.T) {
	m := &writeMock{}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	repo, err := c.CreateRepository(context.Background(), "platform-infra")
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if repo == nil || repo.Name != "platform-infra" {
		t.Fatalf("unexpected repo: %+v", repo)
	}
	if repo.ID == "" {
		t.Error("created repo has empty ID")
	}
	if m.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", m.lastMethod)
	}
	if m.lastContent != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", m.lastContent)
	}
	body := m.lastBody["/_apis/git/repositories"]
	if body["name"] != "platform-infra" {
		t.Errorf("body.name = %v, want platform-infra", body["name"])
	}
	proj, _ := body["project"].(map[string]any)
	if proj["name"] != testProject {
		t.Errorf("body.project.name = %v, want %s", proj["name"], testProject)
	}
}

// TestCreateRepository_Conflict verifies a 409 is surfaced as an *APIError that
// IsConflict recognises (the orchestrator's idempotency hook).
func TestCreateRepository_Conflict(t *testing.T) {
	m := &writeMock{statusByPath: map[string]int{"/_apis/git/repositories": http.StatusConflict}}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	_, err := c.CreateRepository(context.Background(), "platform-infra")
	if err == nil {
		t.Fatal("expected error on 409")
	}
	if !IsConflict(err) {
		t.Errorf("IsConflict(err) = false, want true; err = %v", err)
	}
}

// TestCreateRepository_ServerError verifies a non-conflict failure is returned
// and is not mistaken for a conflict.
func TestCreateRepository_ServerError(t *testing.T) {
	m := &writeMock{statusByPath: map[string]int{"/_apis/git/repositories": http.StatusInternalServerError}}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	_, err := c.CreateRepository(context.Background(), "platform-infra")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if IsConflict(err) {
		t.Error("IsConflict(err) = true on 500, want false")
	}
}

func TestCreatePipeline_Success(t *testing.T) {
	m := &writeMock{}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	pipe, err := c.CreatePipeline(context.Background(), CreatePipelineRequest{
		Name:         "platform-infra-CI",
		Folder:       "\\Infrastructure",
		YAMLPath:     "azure-pipelines.yml",
		RepositoryID: "9a8b7c6d-5e4f-4a3b-2c1d-0e9f8a7b6c5d",
	})
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	if pipe.ID != 42 || pipe.Name != "platform-infra-CI" {
		t.Fatalf("unexpected pipeline: %+v", pipe)
	}
	body := m.lastBody["/_apis/pipelines"]
	cfg, _ := body["configuration"].(map[string]any)
	if cfg["type"] != "yaml" {
		t.Errorf("configuration.type = %v, want yaml", cfg["type"])
	}
	if cfg["path"] != "azure-pipelines.yml" {
		t.Errorf("configuration.path = %v, want azure-pipelines.yml", cfg["path"])
	}
	repo, _ := cfg["repository"].(map[string]any)
	if repo["id"] != "9a8b7c6d-5e4f-4a3b-2c1d-0e9f8a7b6c5d" {
		t.Errorf("configuration.repository.id = %v", repo["id"])
	}
}

func TestCreatePipeline_Conflict(t *testing.T) {
	m := &writeMock{statusByPath: map[string]int{"/_apis/pipelines": http.StatusConflict}}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	_, err := c.CreatePipeline(context.Background(), CreatePipelineRequest{Name: "x"})
	if err == nil || !IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestAdoptBranchPolicy_Success(t *testing.T) {
	m := &writeMock{}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	settings := json.RawMessage(`{"minimumApproverCount":2}`)
	pol, err := c.AdoptBranchPolicy(context.Background(), AdoptBranchPolicyRequest{
		TypeID:     "fa4e907d-c16b-4a4c-9dfa-4906e5d171dd",
		IsEnabled:  true,
		IsBlocking: true,
		Settings:   settings,
	})
	if err != nil {
		t.Fatalf("AdoptBranchPolicy: %v", err)
	}
	if pol.ID != 101 {
		t.Errorf("policy.ID = %d, want 101", pol.ID)
	}
	if !pol.IsEnabled {
		t.Error("policy.IsEnabled = false, want true")
	}
	body := m.lastBody["/_apis/policy/configurations"]
	tp, _ := body["type"].(map[string]any)
	if tp["id"] != "fa4e907d-c16b-4a4c-9dfa-4906e5d171dd" {
		t.Errorf("body.type.id = %v", tp["id"])
	}
}

func TestAdoptBranchPolicy_Conflict(t *testing.T) {
	m := &writeMock{statusByPath: map[string]int{"/_apis/policy/configurations": http.StatusConflict}}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	_, err := c.AdoptBranchPolicy(context.Background(), AdoptBranchPolicyRequest{TypeID: "t"})
	if err == nil || !IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

// TestAdoptVariableGroup_Success verifies the group is created with empty
// placeholder values for every supplied name (no secret values are copied).
func TestAdoptVariableGroup_Success(t *testing.T) {
	m := &writeMock{}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	names := []string{"TF_BACKEND_RESOURCE_GROUP", "TF_BACKEND_STORAGE_ACCOUNT", "ARM_CLIENT_SECRET"}
	vg, err := c.AdoptVariableGroup(context.Background(), "platform-shared", names)
	if err != nil {
		t.Fatalf("AdoptVariableGroup: %v", err)
	}
	if vg.ID != 77 || vg.Name != "platform-shared" {
		t.Fatalf("unexpected group: %+v", vg)
	}
	body := m.lastBody["/_apis/distributedtask/variablegroups"]
	vars, _ := body["variables"].(map[string]any)
	if len(vars) != 3 {
		t.Fatalf("got %d variables in body, want 3", len(vars))
	}
	for _, n := range names {
		v, ok := vars[n].(map[string]any)
		if !ok {
			t.Fatalf("variable %q missing from body", n)
		}
		if v["value"] != "" {
			t.Errorf("variable %q value = %v, want empty placeholder", n, v["value"])
		}
	}
}

func TestAdoptVariableGroup_Conflict(t *testing.T) {
	m := &writeMock{statusByPath: map[string]int{"/_apis/distributedtask/variablegroups": http.StatusConflict}}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	_, err := c.AdoptVariableGroup(context.Background(), "g", []string{"A"})
	if err == nil || !IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestAdoptServiceConnection_Success(t *testing.T) {
	m := &writeMock{}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	conn, err := c.AdoptServiceConnection(context.Background(), AdoptServiceConnectionRequest{
		Name: "azure-prod-connection",
		Type: "azurerm",
		URL:  "https://management.azure.com/",
	})
	if err != nil {
		t.Fatalf("AdoptServiceConnection: %v", err)
	}
	if conn.Name != "azure-prod-connection" || conn.Type != "azurerm" {
		t.Fatalf("unexpected connection: %+v", conn)
	}
	body := m.lastBody["/_apis/serviceendpoint/endpoints"]
	refs, _ := body["serviceEndpointProjectReferences"].([]any)
	if len(refs) != 1 {
		t.Fatalf("got %d project references, want 1", len(refs))
	}
	// The adopted connection must not carry any authorization/credential block.
	if _, present := body["authorization"]; present {
		t.Error("adopted service connection body unexpectedly carries authorization data")
	}
}

func TestAdoptServiceConnection_Conflict(t *testing.T) {
	m := &writeMock{statusByPath: map[string]int{"/_apis/serviceendpoint/endpoints": http.StatusConflict}}
	srv := newWriteServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	_, err := c.AdoptServiceConnection(context.Background(), AdoptServiceConnectionRequest{Name: "c"})
	if err == nil || !IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
}
