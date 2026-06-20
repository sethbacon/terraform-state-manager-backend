package api

import (
	"net/http"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// TestDriftWorkflowTemplates_CaptureModuleProvenance guards that BOTH dispatched
// CI templates upload module provenance (the configuration's module_calls and the
// resolved modules.json) on the existing jq -n callback payload — via --argjson,
// so the values can't be shell/JSON-injected.
func TestDriftWorkflowTemplates_CaptureModuleProvenance(t *testing.T) {
	templates := map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline}
	wants := []string{
		".terraform/modules/modules.json",                       // read the resolved module lockfile
		"module_calls:(.configuration.root_module.module_calls", // extract the module calls subset
		`--argjson plan "$MODULE_CALLS"`,                        // injected, never concatenated
		`--argjson module_locks "$MODULE_LOCKS"`,
		"plan:$plan, module_locks:$module_locks", // present in the posted object
	}
	for name, tmpl := range templates {
		for _, want := range wants {
			if !strings.Contains(tmpl, want) {
				t.Errorf("%s template missing %q", name, want)
			}
		}
	}
}

// TestBuiltinWorkflow_ProfileRouting verifies that profile="suite" returns the
// suite variants and any other profile returns the dependency-free built-ins,
// for every (provider, kind) combination.
func TestBuiltinWorkflow_ProfileRouting(t *testing.T) {
	cases := []struct {
		provider, kind, profile string
		want                    string
	}{
		{"github_actions", "drift", "default", githubDriftWorkflow},
		{"github_actions", "drift", "suite", githubDriftWorkflowSuite},
		{"azure_devops", "drift", "default", azureDriftPipeline},
		{"azure_devops", "drift", "suite", azureDriftPipelineSuite},
		{"github_actions", "versionlab", "default", githubHealthWorkflow},
		{"github_actions", "versionlab", "suite", githubHealthWorkflowSuite},
		{"azure_devops", "versionlab", "default", azureHealthPipeline},
		{"azure_devops", "versionlab", "suite", azureHealthPipelineSuite},
		{"github_actions", "drift", "unknown", githubDriftWorkflow}, // unknown profile -> built-in
	}
	for _, c := range cases {
		if got := builtinWorkflow(c.provider, c.kind, c.profile); got != c.want {
			t.Errorf("builtinWorkflow(%q,%q,%q) returned the wrong template", c.provider, c.kind, c.profile)
		}
	}
}

// TestSuiteWorkflowTemplates_UsePublishedComponents guards that the suite
// variants reference the published Terraform-suite CI components (and, for
// drift, drop the inline jq summarizer entirely in favor of the report
// action/task).
func TestSuiteWorkflowTemplates_UsePublishedComponents(t *testing.T) {
	checks := []struct {
		name, tmpl string
		wants      []string
		absent     string
	}{
		{"github drift suite", githubDriftWorkflowSuite,
			[]string{"sethbacon/setup-terraform-hardened@v1", "sethbacon/terraform-drift-report@v1"}, "jq "},
		{"azure drift suite", azureDriftPipelineSuite,
			[]string{"PipelineTerraformInstaller@1", "PipelineTerraformDriftReport@1"}, "jq "},
		{"github versionlab suite", githubHealthWorkflowSuite,
			[]string{"sethbacon/setup-terraform-hardened@v1", "sethbacon/terraform-provider-mirror@v1"}, ""},
		{"azure versionlab suite", azureHealthPipelineSuite,
			[]string{"PipelineTerraformInstaller@1", "PipelineTerraformProviderMirror@1"}, ""},
	}
	for _, c := range checks {
		for _, w := range c.wants {
			if !strings.Contains(c.tmpl, w) {
				t.Errorf("%s missing %q", c.name, w)
			}
		}
		if c.absent != "" && strings.Contains(c.tmpl, c.absent) {
			t.Errorf("%s should not contain %q (the report action replaces inline jq)", c.name, c.absent)
		}
	}
}

// TestRunResults_CapturesModuleProvenance verifies that a dispatched-run callback
// carrying the optional plan (module calls) + module_locks records module
// provenance against the run's own source/state, with the locked version filled
// from the lockfile.
func TestRunResults_CapturesModuleProvenance(t *testing.T) {
	e := newDriftEnv(t)

	runRow := sqlmock.NewRows(driftCols).
		AddRow("d1", "p1", "s1", "envs/prod.tfstate", "", "", "dispatched",
			nil, nil, nil, nil, nil, "", "tokX", "alice", "2026-06-11", "2026-06-11")
	e.mock.ExpectQuery("FROM drift_runs WHERE id").WithArgs("d1").WillReturnRows(runRow)
	e.mock.ExpectExec("UPDATE drift_runs SET callback_token=''").WithArgs("d1", "tokX").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE drift_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	// Clean callback: the record layer resolves.
	e.mock.ExpectExec("UPDATE drift_records SET status='resolved'").WithArgs("s1", "envs/prod.tfstate").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Capture: ReplaceForState tx writes one ref with the version from the lockfile.
	e.mock.ExpectBegin()
	e.mock.ExpectExec("DELETE FROM state_module_refs").WithArgs("s1", "envs/prod.tfstate").
		WillReturnResult(sqlmock.NewResult(0, 0))
	e.mock.ExpectExec("INSERT INTO state_module_refs").
		WithArgs("s1", "envs/prod.tfstate", "acme/vpc/aws", "5.3.0", "registry.terraform.io").
		WillReturnResult(sqlmock.NewResult(1, 1))
	e.mock.ExpectCommit()

	body := `{"drifted":false,
		"plan":{"configuration":{"root_module":{"module_calls":{"vpc":{"source":"acme/vpc/aws","version_constraint":"~> 5.0"}}}}},
		"module_locks":{"Modules":[{"Key":"vpc","Source":"acme/vpc/aws","Version":"5.3.0"}]}}`
	w := e.doWithHeader(http.MethodPost, "/api/v1/drift/runs/d1/results", body, "X-TSM-Callback-Token", "tokX")
	if w.Code != http.StatusOK {
		t.Fatalf("callback: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected module provenance to be captured: %v", err)
	}
}
