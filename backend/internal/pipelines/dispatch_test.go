package pipelines

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	_, err := DispatchGitHubDrift(context.Background(), "ghp_tok", cfg, "",
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
	_, err := DispatchAzureDevOpsDrift(context.Background(), ADOPAT("pat-secret"), cfg, "feature/x",
		DriftInputs{CallbackURL: "https://tsm/cb", CallbackToken: "cbt"}, nil)
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

// TestDispatchAzureDevOps_WireBody_NoTargets_MatchesTodayExactly is the golden
// test for the fan-out dispatch change (drift-fleet-scale.md Phase 1, design
// decision #3: "wire back-compat is absolute"). Written and run against the
// UNMODIFIED dispatch code before any of it changed, it proves that a
// no-targets request sends EXACTLY the three keys it sends today --
// callback_url, callback_token, working_dir -- and nothing else. Any later
// change that starts sending a fourth key (e.g. "targets") for this shape
// breaks ADO pipelines that declare only the three legacy parameters
// ("Unexpected parameter").
func TestDispatchAzureDevOps_WireBody_NoTargets_MatchesTodayExactly(t *testing.T) {
	var gotBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	cfg := AzureDevOpsConfig{Organization: "corp", Project: "Platform", PipelineID: "42"}
	_, err := DispatchAzureDevOpsDrift(context.Background(), ADOPAT("pat-secret"), cfg, "",
		DriftInputs{CallbackURL: "https://tsm/cb", CallbackToken: "cbt", WorkingDir: "infra/"}, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var params map[string]string
	if err := json.Unmarshal(gotBody["templateParameters"], &params); err != nil {
		t.Fatalf("templateParameters did not decode: %v", err)
	}
	want := map[string]string{
		"callback_url":   "https://tsm/cb",
		"callback_token": "cbt",
		"working_dir":    "infra/",
	}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("templateParameters = %v, want EXACTLY %v (no more, no fewer keys)", params, want)
	}
}

// TestDispatchAzureDevOps_WireBody_WithTargets_AddsParam is the fan-out twin of
// the golden test above: when the caller fills TargetsJSON (2+ targets), the
// wire body gains a FOURTH key, "targets", carrying that JSON verbatim --
// alongside the same three legacy keys, taken from the first target, so a
// pipeline that has not adopted `targets` yet still gets a request it
// understands.
func TestDispatchAzureDevOps_WireBody_WithTargets_AddsParam(t *testing.T) {
	var gotBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	const targetsJSON = `[{"working_dir":"app1/","state_key":"app1.tfstate","callback_url":"https://tsm/r1","callback_token":"t1"},` +
		`{"working_dir":"app2/","state_key":"app2.tfstate","callback_url":"https://tsm/r2","callback_token":"t2"}]`

	cfg := AzureDevOpsConfig{Organization: "corp", Project: "Platform", PipelineID: "42"}
	_, err := DispatchAzureDevOpsDrift(context.Background(), ADOPAT("pat-secret"), cfg, "",
		DriftInputs{CallbackURL: "https://tsm/r1", CallbackToken: "t1", WorkingDir: "app1/", TargetsJSON: targetsJSON}, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var params map[string]string
	if err := json.Unmarshal(gotBody["templateParameters"], &params); err != nil {
		t.Fatalf("templateParameters did not decode: %v", err)
	}
	want := map[string]string{
		"callback_url":   "https://tsm/r1",
		"callback_token": "t1",
		"working_dir":    "app1/",
		"targets":        targetsJSON,
	}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("templateParameters = %v, want EXACTLY %v", params, want)
	}
}

// TestDispatchAzureDevOpsRun_AddsVariablesKey_OnlyWhenNonEmpty pins the wire
// shape of the OTHER half of the Phase 1b item 3 fix, at the pipelines-package
// level: a non-empty variables map adds a "variables" key alongside
// templateParameters (never merged into it -- a run variable and a template
// parameter are resolved at different times), and a nil/empty one omits the
// key entirely rather than sending "variables":{}.
func TestDispatchAzureDevOpsRun_AddsVariablesKey_OnlyWhenNonEmpty(t *testing.T) {
	var gotBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	cfg := AzureDevOpsConfig{Organization: "corp", Project: "Platform", PipelineID: "42"}

	// Nil variables: no "variables" key at all.
	if _, err := DispatchAzureDevOpsRun(context.Background(), ADOPAT("pat"), cfg, "", map[string]string{"working_dir": "."}, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, hasVariables := gotBody["variables"]; hasVariables {
		t.Fatalf("nil variables must omit the key entirely, got: %v", gotBody)
	}

	// Populated variables: the key appears, verbatim, alongside templateParameters.
	vars := map[string]ADORunVariable{"cb_token_app1_": {Value: "secret-tok", IsSecret: true}}
	if _, err := DispatchAzureDevOpsRun(context.Background(), ADOPAT("pat"), cfg, "", map[string]string{"working_dir": "."}, vars); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var gotVars map[string]ADORunVariable
	if err := json.Unmarshal(gotBody["variables"], &gotVars); err != nil {
		t.Fatalf("variables did not decode: %v", err)
	}
	if !reflect.DeepEqual(gotVars, vars) {
		t.Fatalf("variables = %+v, want %+v", gotVars, vars)
	}
}

// TestDispatchAzureDevOpsRun_DecodesRunIDAndWebLink_On201 pins the response
// decode DispatchAzureDevOpsRun adds: a 201 body carrying an id and a web link
// yields a populated CIRunRef.
func TestDispatchAzureDevOpsRun_DecodesRunIDAndWebLink_On201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":12345,"_links":{"web":{"href":"https://dev.azure.com/corp/p/_build/results?buildId=12345"}}}`))
	}))
	defer srv.Close()
	old := azureDevOpsBaseURL
	azureDevOpsBaseURL = srv.URL
	defer func() { azureDevOpsBaseURL = old }()

	cfg := AzureDevOpsConfig{Organization: "corp", Project: "P", PipelineID: "1"}
	ref, err := DispatchAzureDevOpsRun(context.Background(), ADOPAT("pat"), cfg, "", map[string]string{"a": "b"}, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if ref == nil || ref.ID != "12345" || ref.WebURL != "https://dev.azure.com/corp/p/_build/results?buildId=12345" {
		t.Fatalf("CIRunRef = %+v, want id=12345 and the web link", ref)
	}
}

// TestDispatchAzureDevOpsRun_MalformedBody_NilRefNoError pins that a missing or
// malformed response body on a 200/201 is NOT an error -- the dispatch itself
// already succeeded, and the run id/link are best-effort.
func TestDispatchAzureDevOpsRun_MalformedBody_NilRefNoError(t *testing.T) {
	cfg := AzureDevOpsConfig{Organization: "corp", Project: "P", PipelineID: "1"}

	for name, body := range map[string]string{
		"empty body":       "",
		"not json":         "not json at all",
		"json but no id":   `{"name":"whatever"}`,
		"id present as ''": `{"id":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			old := azureDevOpsBaseURL
			azureDevOpsBaseURL = srv.URL
			defer func() { azureDevOpsBaseURL = old }()

			ref, err := DispatchAzureDevOpsRun(context.Background(), ADOPAT("pat"), cfg, "", nil, nil)
			if err != nil {
				t.Fatalf("a malformed success body must not be an error: %v", err)
			}
			if ref != nil {
				t.Fatalf("expected a nil CIRunRef, got %+v", ref)
			}
		})
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
