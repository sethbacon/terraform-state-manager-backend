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

// runMock is an httptest-backed mock of the pipeline-runs write surface. It
// asserts the request is a POST with a JSON body, captures the decoded body and
// the request path, and serves the run-created fixture (or an injected status).
type runMock struct {
	status     int // status to return instead of 201; 0 means created
	lastBody   map[string]any
	lastMethod string
	lastPath   string
}

func newRunServer(t *testing.T, m *runMock) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.lastMethod = r.Method
		m.lastPath = r.URL.Path

		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &m.lastBody)

		if !strings.Contains(r.URL.Path, "/_apis/pipelines/") || !strings.HasSuffix(r.URL.Path, "/runs") {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if m.status != 0 {
			http.Error(w, `{"message":"injected"}`, m.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(loadFixture(t, "pipeline_run_created.json"))
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestQueuePipelineRun_Success(t *testing.T) {
	m := &runMock{}
	srv := newRunServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	run, err := c.QueuePipelineRun(context.Background(), QueuePipelineRunRequest{
		PipelineID: 42,
		Branch:     "refs/heads/main",
		Parameters: map[string]string{"mode": "plan"},
	})
	if err != nil {
		t.Fatalf("QueuePipelineRun: %v", err)
	}
	if run.ID != 1234 || run.State != "inProgress" {
		t.Fatalf("unexpected run: %+v", run)
	}
	if m.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", m.lastMethod)
	}
	// Path must target the pipeline-scoped runs endpoint.
	if !strings.Contains(m.lastPath, "/_apis/pipelines/42/runs") {
		t.Errorf("path = %q, want .../_apis/pipelines/42/runs", m.lastPath)
	}
	// Branch maps onto resources.repositories.self.refName.
	res, _ := m.lastBody["resources"].(map[string]any)
	repos, _ := res["repositories"].(map[string]any)
	self, _ := repos["self"].(map[string]any)
	if self["refName"] != "refs/heads/main" {
		t.Errorf("refName = %v, want refs/heads/main", self["refName"])
	}
	// Parameters map onto templateParameters.
	params, _ := m.lastBody["templateParameters"].(map[string]any)
	if params["mode"] != "plan" {
		t.Errorf("templateParameters.mode = %v, want plan", params["mode"])
	}
}

// TestQueuePipelineRun_MinimalBody verifies a parameter-less, default-branch run
// sends a minimal body with no resources or templateParameters keys.
func TestQueuePipelineRun_MinimalBody(t *testing.T) {
	m := &runMock{}
	srv := newRunServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	if _, err := c.QueuePipelineRun(context.Background(), QueuePipelineRunRequest{PipelineID: 7}); err != nil {
		t.Fatalf("QueuePipelineRun: %v", err)
	}
	if _, ok := m.lastBody["resources"]; ok {
		t.Error("minimal run body unexpectedly carries resources")
	}
	if _, ok := m.lastBody["templateParameters"]; ok {
		t.Error("minimal run body unexpectedly carries templateParameters")
	}
}

func TestQueuePipelineRun_RequiresPipelineID(t *testing.T) {
	c := newTestClient(t, "https://x", testPAT)
	if _, err := c.QueuePipelineRun(context.Background(), QueuePipelineRunRequest{PipelineID: 0}); err == nil {
		t.Fatal("expected error when PipelineID is unset")
	}
}

func TestQueuePipelineRun_ServerError(t *testing.T) {
	m := &runMock{status: http.StatusInternalServerError}
	srv := newRunServer(t, m)
	c := newTestClient(t, srv.URL, testPAT)

	_, err := c.QueuePipelineRun(context.Background(), QueuePipelineRunRequest{PipelineID: 42})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if IsConflict(err) {
		t.Error("IsConflict(err) = true on 500, want false")
	}
}
