package pipelines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListAzurePipelines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/myorg/myproj/_apis/pipelines" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing auth header")
		}
		_, _ = w.Write([]byte(`{"count":2,"value":[{"id":7,"name":"drift","folder":"\\"},{"id":3,"name":"build","folder":"\\ci"}]}`))
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	refs, err := ListAzurePipelines(context.Background(), "pat", "myorg", "myproj")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Name != "build" || refs[1].ID != 7 {
		t.Fatalf("unexpected refs: %+v", refs)
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
