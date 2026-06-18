package pipelines

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetupAzureWorkflowCreatesBranchAndPR(t *testing.T) {
	var pushedBranch, prSource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/o/p/_apis/git/repositories/r1" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"defaultBranch":"refs/heads/develop"}`))
		case r.URL.Path == "/o/p/_apis/git/repositories/r1/items":
			w.WriteHeader(http.StatusNotFound) // file not present yet
		case r.URL.Path == "/o/p/_apis/git/repositories/r1/refs":
			_, _ = w.Write([]byte(`{"value":[{"name":"refs/heads/develop","objectId":"abc123"}]}`))
		case r.URL.Path == "/o/p/_apis/git/repositories/r1/pushes" && r.Method == http.MethodPost:
			var push struct {
				RefUpdates []struct {
					Name        string `json:"name"`
					OldObjectID string `json:"oldObjectId"`
				} `json:"refUpdates"`
				Commits []struct {
					Changes []struct {
						Item struct {
							Path string `json:"path"`
						} `json:"item"`
					} `json:"changes"`
				} `json:"commits"`
			}
			_ = json.NewDecoder(r.Body).Decode(&push)
			if push.RefUpdates[0].OldObjectID != "abc123" {
				t.Errorf("push not based on default branch tip: %+v", push.RefUpdates)
			}
			if push.Commits[0].Changes[0].Item.Path != AzureWorkflowPath {
				t.Errorf("wrong path: %s", push.Commits[0].Changes[0].Item.Path)
			}
			pushedBranch = push.RefUpdates[0].Name
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/o/p/_apis/git/repositories/r1/pullrequests" && r.Method == http.MethodPost:
			var pr struct {
				SourceRefName string `json:"sourceRefName"`
				TargetRefName string `json:"targetRefName"`
			}
			_ = json.NewDecoder(r.Body).Decode(&pr)
			prSource = pr.SourceRefName
			if pr.TargetRefName != "refs/heads/develop" {
				t.Errorf("PR target should be the default branch, got %s", pr.TargetRefName)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"pullRequestId":42,"repository":{"webUrl":"https://dev.azure.com/o/p/_git/r1"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	res, err := SetupAzureWorkflow(context.Background(), ADOPAT("pat"), "o", "p", "r1",
		[]FileSpec{{Path: AzureWorkflowPath, Content: "yaml-content"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "pr_created" || res.PRID != 42 || !strings.Contains(res.PRURL, "/pullrequest/42") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if pushedBranch != "refs/heads/"+res.Branch || prSource != pushedBranch {
		t.Fatalf("branch mismatch: pushed=%s pr=%s result=%s", pushedBranch, prSource, res.Branch)
	}
}

func TestSetupAzureWorkflowIdempotentWhenFileExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repositories/r1"):
			_, _ = w.Write([]byte(`{"defaultBranch":"refs/heads/main"}`))
		case strings.HasSuffix(r.URL.Path, "/items"):
			_, _ = w.Write([]byte(`{"objectId":"x"}`)) // 200 = exists
		default:
			t.Errorf("should not write anything, got %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	res, err := SetupAzureWorkflow(context.Background(), ADOPAT("pat"), "o", "p", "r1",
		[]FileSpec{{Path: AzureWorkflowPath, Content: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "exists" {
		t.Fatalf("expected exists, got %+v", res)
	}
}

func TestSetupGitHubWorkflowCreatesBranchAndPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/octocat/infra" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case strings.HasPrefix(r.URL.Path, "/repos/octocat/infra/contents/") && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/repos/octocat/infra/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"deadbeef"}}`))
		case r.URL.Path == "/repos/octocat/infra/git/refs" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/repos/octocat/infra/contents/") && r.Method == http.MethodPut:
			var put struct {
				Content string `json:"content"`
				Branch  string `json:"branch"`
			}
			_ = json.NewDecoder(r.Body).Decode(&put)
			decoded, _ := base64.StdEncoding.DecodeString(put.Content)
			if string(decoded) != "wf-yaml" {
				t.Errorf("content not base64 round-tripped: %q", decoded)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/repos/octocat/infra/pulls" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/octocat/infra/pull/7"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	defer func() { githubAPIBaseURL = old }()

	res, err := SetupGitHubWorkflow(context.Background(), "tok", "octocat", "infra",
		[]FileSpec{{Path: GitHubWorkflowPath, Content: "wf-yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "pr_created" || res.PRID != 7 || res.PRURL == "" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestPRStateNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/pullrequests/1"):
			_, _ = w.Write([]byte(`{"status":"completed"}`))
		case strings.Contains(r.URL.Path, "/pullrequests/2"):
			_, _ = w.Write([]byte(`{"status":"active"}`))
		case strings.Contains(r.URL.Path, "/pulls/3"):
			_, _ = w.Write([]byte(`{"state":"closed","merged":true}`))
		case strings.Contains(r.URL.Path, "/pulls/4"):
			_, _ = w.Write([]byte(`{"state":"closed","merged":false}`))
		}
	}))
	defer srv.Close()
	oldA, oldG := azureDevOpsBaseURL, githubAPIBaseURL
	azureDevOpsBaseURL, githubAPIBaseURL = srv.URL, srv.URL
	defer func() { azureDevOpsBaseURL, githubAPIBaseURL = oldA, oldG }()

	ctx := context.Background()
	if s, _ := AzurePRState(ctx, ADOPAT("p"), "o", "p", "r", 1); s != "merged" {
		t.Errorf("ado completed → %s", s)
	}
	if s, _ := AzurePRState(ctx, ADOPAT("p"), "o", "p", "r", 2); s != "open" {
		t.Errorf("ado active → %s", s)
	}
	if s, _ := GitHubPRState(ctx, "t", "o", "r", 3); s != "merged" {
		t.Errorf("gh merged → %s", s)
	}
	if s, _ := GitHubPRState(ctx, "t", "o", "r", 4); s != "closed" {
		t.Errorf("gh closed-unmerged → %s", s)
	}
}

func TestSetupAzureWorkflowMultiFileSkipsExisting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repositories/r1") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"defaultBranch":"refs/heads/main"}`))
		case strings.HasSuffix(r.URL.Path, "/items"):
			// drift file exists, versionlab does not
			if strings.Contains(r.URL.RawQuery, "drift") {
				_, _ = w.Write([]byte(`{"objectId":"x"}`))
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case strings.HasSuffix(r.URL.Path, "/refs"):
			_, _ = w.Write([]byte(`{"value":[{"name":"refs/heads/main","objectId":"tip1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/pushes"):
			var push struct {
				Commits []struct {
					Changes []struct {
						Item struct {
							Path string `json:"path"`
						} `json:"item"`
					} `json:"changes"`
				} `json:"commits"`
			}
			_ = json.NewDecoder(r.Body).Decode(&push)
			if len(push.Commits[0].Changes) != 1 || push.Commits[0].Changes[0].Item.Path != WorkflowPaths["versionlab"].Azure {
				t.Errorf("expected only the missing versionlab file, got %+v", push.Commits[0].Changes)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/pullrequests"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"pullRequestId":5,"repository":{"webUrl":"https://x/_git/r1"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	res, err := SetupAzureWorkflow(context.Background(), ADOPAT("pat"), "o", "p", "r1", []FileSpec{
		{Path: WorkflowPaths["drift"].Azure, Content: "a"},
		{Path: WorkflowPaths["versionlab"].Azure, Content: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "pr_created" || res.PRID != 5 {
		t.Fatalf("unexpected: %+v", res)
	}
}
