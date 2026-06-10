package pipelines

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListAzurePipelinesFollowsContinuation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/myorg/myproj/_apis/pipelines" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing auth header")
		}
		// First page carries a continuation token; the second is final.
		if r.URL.Query().Get("continuationToken") == "" {
			w.Header().Set("X-MS-ContinuationToken", "page2")
			_, _ = w.Write([]byte(`{"count":2,"value":[{"id":7,"name":"drift","folder":"\\"},{"id":3,"name":"build","folder":"\\ci"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"value":[{"id":9,"name":"release","folder":"\\"}]}`))
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	refs, err := ListAzurePipelines(context.Background(), "pat", "myorg", "myproj")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs[0].Name != "build" || refs[2].Name != "release" {
		t.Fatalf("expected 3 pipelines across pages, got: %+v", refs)
	}

	if _, err := ListAzurePipelines(context.Background(), "", "o", "p"); err == nil {
		t.Fatal("empty PAT accepted")
	}
}

func TestListGitHubReposOrgFallsBackToUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/octocat/repos":
			w.WriteHeader(http.StatusNotFound)
		case "/users/octocat/repos":
			_, _ = w.Write([]byte(`[{"name":"zeta","default_branch":"main"},{"name":"alpha","default_branch":"master"}]`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	defer func() { githubAPIBaseURL = old }()

	repos, err := ListGitHubRepos(context.Background(), "tok", "octocat")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Name != "alpha" || repos[1].DefaultBranch != "main" {
		t.Fatalf("unexpected repos: %+v", repos)
	}
}

func TestListGitHubReposPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/bigorg/repos" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		// Page 1: a full page of 100 generated repos; page 2: one more (short → stop).
		if r.URL.Query().Get("page") == "1" {
			items := make([]string, 0, 100)
			for i := 0; i < 100; i++ {
				items = append(items, fmt.Sprintf(`{"name":"repo-%03d","default_branch":"main"}`, i))
			}
			_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
			return
		}
		_, _ = w.Write([]byte(`[{"name":"repo-zzz","default_branch":"main"}]`))
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	defer func() { githubAPIBaseURL = old }()

	repos, err := ListGitHubRepos(context.Background(), "tok", "bigorg")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 101 || repos[100].Name != "repo-zzz" {
		t.Fatalf("expected 101 repos across pages, got %d", len(repos))
	}
}

func TestListGitHubWorkflowsFiltersAndFileNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octocat/infra/actions/workflows" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"workflows":[
			{"id":1,"name":"Drift","path":".github/workflows/tsm-drift.yml","state":"active"},
			{"id":2,"name":"Old","path":".github/workflows/old.yml","state":"disabled_manually"}
		]}`))
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	defer func() { githubAPIBaseURL = old }()

	wfs, err := ListGitHubWorkflows(context.Background(), "tok", "octocat", "infra")
	if err != nil {
		t.Fatal(err)
	}
	if len(wfs) != 1 || wfs[0].File != "tsm-drift.yml" || wfs[0].Name != "Drift" {
		t.Fatalf("unexpected workflows: %+v", wfs)
	}
}

func TestListAzureReposAndServiceConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/p/_apis/git/repositories":
			_, _ = w.Write([]byte(`{"value":[{"id":"r2","name":"zeta","defaultBranch":"refs/heads/main"},{"id":"r1","name":"app","defaultBranch":"refs/heads/develop"}]}`))
		case "/o/p/_apis/serviceendpoint/endpoints":
			_, _ = w.Write([]byte(`{"value":[{"id":"sc1","name":"azure-prod","type":"azurerm"}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	repos, err := ListAzureRepos(context.Background(), "pat", "o", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[0].Name != "app" || repos[1].ID != "r2" {
		t.Fatalf("unexpected repos: %+v", repos)
	}
	scs, err := ListAzureServiceConnections(context.Background(), "pat", "o", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(scs) != 1 || scs[0].Name != "azure-prod" || scs[0].Type != "azurerm" {
		t.Fatalf("unexpected service connections: %+v", scs)
	}
}

func TestCreateAzurePipeline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/o/p/_apis/pipelines" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Name          string `json:"name"`
			Configuration struct {
				Type       string `json:"type"`
				Path       string `json:"path"`
				Repository struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"repository"`
			} `json:"configuration"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name != "TSM Drift" || body.Configuration.Path != "/azure-pipelines-tsm-drift.yml" ||
			body.Configuration.Repository.ID != "r1" || body.Configuration.Type != "yaml" {
			t.Errorf("unexpected payload: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"name":"TSM Drift","folder":"\\"}`))
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	ref, err := CreateAzurePipeline(context.Background(), "pat", "o", "p", "TSM Drift", "/azure-pipelines-tsm-drift.yml", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != 99 || ref.Name != "TSM Drift" {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	if _, err := CreateAzurePipeline(context.Background(), "pat", "o", "p", "", "/x.yml", "r1"); err == nil {
		t.Fatal("empty name accepted")
	}
}
