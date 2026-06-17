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

// ---------------------------------------------------------------------------
// GitHub Actions dispatch
// ---------------------------------------------------------------------------

func TestDispatchGitHub_Validation(t *testing.T) {
	ctx := context.Background()
	if err := DispatchGitHub(ctx, "tok", GitHubConfig{Repo: "r", WorkflowID: "w"}, "", nil); err == nil {
		t.Error("missing owner must error")
	}
	if err := DispatchGitHub(ctx, "", GitHubConfig{Owner: "o", Repo: "r", WorkflowID: "w"}, "", nil); err == nil {
		t.Error("missing token must error")
	}
}

func TestDispatchGitHub_SendsWorkflowDispatch(t *testing.T) {
	var gotPath, gotAuth, gotRef string
	var gotInputs map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Ref    string            `json:"ref"`
			Inputs map[string]string `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotRef, gotInputs = body.Ref, body.Inputs
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	defer func() { githubAPIBaseURL = old }()

	cfg := GitHubConfig{Owner: "org", Repo: "infra", WorkflowID: "tsm-drift.yml", Ref: "develop"}
	err := DispatchGitHubDrift(context.Background(), "ghp_tok", cfg, "",
		DriftInputs{CallbackURL: "https://tsm/cb", CallbackToken: "cbt", WorkingDir: "envs/prod"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotPath != "/repos/org/infra/actions/workflows/tsm-drift.yml/dispatches" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer ghp_tok" {
		t.Errorf("auth = %s", gotAuth)
	}
	if gotRef != "develop" {
		t.Errorf("ref should fall back to the connection default: %s", gotRef)
	}
	if gotInputs["callback_url"] != "https://tsm/cb" || gotInputs["callback_token"] != "cbt" || gotInputs["working_dir"] != "envs/prod" {
		t.Errorf("inputs = %v", gotInputs)
	}
}

func TestDispatchGitHub_DefaultsToMainAndSurfacesErrors(t *testing.T) {
	var gotRef string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ref string `json:"ref"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotRef = body.Ref
		http.Error(w, `{"message":"workflow not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	old := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	defer func() { githubAPIBaseURL = old }()

	err := DispatchGitHub(context.Background(), "tok", GitHubConfig{Owner: "o", Repo: "r", WorkflowID: "w"}, "", nil)
	if err == nil || !strings.Contains(err.Error(), "workflow not found") {
		t.Errorf("API error body must surface: %v", err)
	}
	if gotRef != "main" {
		t.Errorf("ref default = %s, want main", gotRef)
	}
}

// ---------------------------------------------------------------------------
// Azure DevOps dispatch
// ---------------------------------------------------------------------------

func TestDispatchAzureDevOps_Validation(t *testing.T) {
	ctx := context.Background()
	if err := DispatchAzureDevOps(ctx, ADOPAT("pat"), AzureDevOpsConfig{Project: "p", PipelineID: "1"}, "", nil); err == nil {
		t.Error("missing organization must error")
	}
	if err := DispatchAzureDevOps(ctx, ADOPAT(""), AzureDevOpsConfig{Organization: "o", Project: "p", PipelineID: "1"}, "", nil); err == nil {
		t.Error("missing PAT must error")
	}
}

type adoRunBody struct {
	TemplateParameters map[string]string `json:"templateParameters"`
	Resources          *struct {
		Repositories struct {
			Self struct {
				RefName string `json:"refName"`
			} `json:"self"`
		} `json:"repositories"`
	} `json:"resources"`
}

func TestDispatchAzureDevOps_NormalizesRefAndAuth(t *testing.T) {
	var gotPath, gotAuth string
	var got adoRunBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	cfg := AzureDevOpsConfig{Organization: "corp", Project: "Platform", PipelineID: "42"}
	err := DispatchAzureDevOpsDrift(context.Background(), ADOPAT("pat-secret"), cfg, "feature/x",
		DriftInputs{CallbackURL: "https://tsm/cb", CallbackToken: "cbt"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotPath != "/corp/Platform/_apis/pipelines/42/runs" {
		t.Errorf("path = %s", gotPath)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":pat-secret"))
	if gotAuth != wantAuth {
		t.Errorf("auth = %s", gotAuth)
	}
	if got.Resources == nil || got.Resources.Repositories.Self.RefName != "refs/heads/feature/x" {
		t.Errorf("bare branch must normalize to refs/heads/: %+v", got.Resources)
	}
	if got.TemplateParameters["callback_token"] != "cbt" {
		t.Errorf("template parameters = %v", got.TemplateParameters)
	}
}

func TestDispatchAzureDevOps_OmitsRefWhenUnset(t *testing.T) {
	var got adoRunBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	cfg := AzureDevOpsConfig{Organization: "corp", Project: "P", PipelineID: "1"}
	if err := DispatchAzureDevOps(context.Background(), ADOPAT("pat"), cfg, "", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// No ref configured anywhere → the run must use the pipeline's own default
	// branch (the old refs/heads/main guess broke differently-named defaults).
	if got.Resources != nil {
		t.Errorf("resources block must be omitted without a ref: %+v", got.Resources)
	}

	// An already-qualified ref passes through untouched.
	if err := DispatchAzureDevOps(context.Background(), ADOPAT("pat"), cfg, "refs/tags/v1", nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.Resources == nil || got.Resources.Repositories.Self.RefName != "refs/tags/v1" {
		t.Errorf("qualified ref must pass through: %+v", got.Resources)
	}
}

func TestDispatchAzureDevOps_SurfacesAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Unable to resolve refs/heads/main"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	cfg := AzureDevOpsConfig{Organization: "corp", Project: "P", PipelineID: "1"}
	err := DispatchAzureDevOps(context.Background(), ADOPAT("pat"), cfg, "main", nil)
	if err == nil || !strings.Contains(err.Error(), "Unable to resolve") {
		t.Errorf("API error body must surface: %v", err)
	}
}
