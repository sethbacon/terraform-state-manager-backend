package pipelines

import (
	"context"
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
