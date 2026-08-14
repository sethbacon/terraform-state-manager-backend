package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"gopkg.in/yaml.v3"

	"github.com/terraform-state-manager/terraform-state-manager/internal/services/driftingest"
)

// TestDriftWorkflowTemplates_CaptureModuleProvenance guards that BOTH dispatched
// CI templates upload module provenance (the configuration's module_calls and the
// resolved modules.json) on the existing jq -n callback payload — via --argjson,
// so the values can't be shell/JSON-injected.
func TestDriftWorkflowTemplates_CaptureModuleProvenance(t *testing.T) {
	templates := map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline}
	wants := []string{
		".terraform/modules/modules.json", // read the resolved module lockfile
		"{module_calls:$o.m}",             // the PROJECTED calls (see the redaction test below)
		`--argjson plan "$MODULE_CALLS"`,  // injected, never concatenated
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

// TestHealthWorkflowTemplates_FailureReportStep guards that every version-lab
// (health) template carries a failure-report step. The happy-path callback runs
// under `set +e` so a normal init/plan failure already posts success:false, but a
// crash in the pre-`set +e` portion (a bad working_dir, the provider-override
// generation) exits before the curl — leaving the run at "dispatched" until the
// reconciler reaps it. The failure step closes that hole: it posts success:false
// with status "completed" (status "failed" is reserved for reconciler expiry) and
// tolerates a 409 (the happy-path curl may have already consumed the one-shot
// token).
func TestHealthWorkflowTemplates_FailureReportStep(t *testing.T) {
	common := []string{
		`success:false`,      // alerts via the !success arm of healthResultFailed
		`status:"completed"`, // NOT "failed" — that is reserved for reconciler expiry
		"409",                // tolerate the one-shot-token race with the happy-path curl
	}
	github := map[string]string{
		"github versionlab":       githubHealthWorkflow,
		"github versionlab suite": githubHealthWorkflowSuite,
	}
	azure := map[string]string{
		"azure versionlab":       azureHealthPipeline,
		"azure versionlab suite": azureHealthPipelineSuite,
	}
	for name, tmpl := range github {
		for _, want := range append([]string{"if: failure()"}, common...) {
			if !strings.Contains(tmpl, want) {
				t.Errorf("%s template missing failure-step marker %q", name, want)
			}
		}
	}
	for name, tmpl := range azure {
		for _, want := range append([]string{"condition: failed()"}, common...) {
			if !strings.Contains(tmpl, want) {
				t.Errorf("%s template missing failure-step marker %q", name, want)
			}
		}
	}
}

// TestWorkflowTemplates_MaskCallbackToken guards that every dispatched CI template
// masks the one-shot callback token in job logs before any other step runs (#258,
// CWE-532). GitHub Actions does not auto-mask workflow_dispatch inputs and Azure
// DevOps does not auto-mask template parameters, so the token — which authenticates
// the result callback — would otherwise be recoverable from verbose / step-debug
// logs. GitHub uses the ::add-mask:: workflow command; Azure registers the value as
// a secret variable (issecret=true) so the agent scrubs it from subsequent output.
func TestWorkflowTemplates_MaskCallbackToken(t *testing.T) {
	github := map[string]string{
		"github drift":            githubDriftWorkflow,
		"github drift suite":      githubDriftWorkflowSuite,
		"github versionlab":       githubHealthWorkflow,
		"github versionlab suite": githubHealthWorkflowSuite,
	}
	azure := map[string]string{
		"azure drift":            azureDriftPipeline,
		"azure drift suite":      azureDriftPipelineSuite,
		"azure versionlab":       azureHealthPipeline,
		"azure versionlab suite": azureHealthPipelineSuite,
	}
	for name, tmpl := range github {
		if !strings.Contains(tmpl, `echo "::add-mask::$CALLBACK_TOKEN"`) {
			t.Errorf("%s template does not mask the callback token via ::add-mask::", name)
		}
	}
	for name, tmpl := range azure {
		if !strings.Contains(tmpl, "issecret=true]$CALLBACK_TOKEN") {
			t.Errorf("%s template does not register the callback token as a secret variable", name)
		}
	}
}

// The shipped provenance assignments, matched verbatim in the template text: the
// shared JQ_REDACT definitions and each of the two fields that use them.
var (
	jqRedactBlockRe    = regexp.MustCompile(`(?s)(JQ_REDACT='.*?')`)
	moduleCallsBlockRe = regexp.MustCompile(`(?s)(MODULE_CALLS=\$\(jq -c "\$JQ_REDACT".*?' plan\.json\))`)
	moduleLocksBlockRe = regexp.MustCompile(`(?s)(MODULE_LOCKS=\$\(.*?echo null \))`)
)

// shippedBlock extracts the shared jq redaction definitions plus one provenance
// assignment verbatim from a dispatched template, so the tests below exercise the
// exact text a runner executes rather than a copy that can drift from it. Both
// fields are scrubbed by the SAME JQ_REDACT block, which is why it is prepended
// rather than restated here.
func shippedBlock(t *testing.T, tmpl string, re *regexp.Regexp, what string) string {
	t.Helper()
	defs := jqRedactBlockRe.FindStringSubmatch(tmpl)
	if defs == nil {
		t.Fatal("template has no JQ_REDACT='…' definitions")
	}
	m := re.FindStringSubmatch(tmpl)
	if m == nil {
		t.Fatalf("template has no %s assignment", what)
	}
	return defs[1] + "\n" + m[1]
}

func moduleCallsBlock(t *testing.T, tmpl string) string {
	t.Helper()
	return shippedBlock(t, tmpl, moduleCallsBlockRe, "MODULE_CALLS=$(jq …)")
}

func moduleLocksBlock(t *testing.T, tmpl string) string {
	t.Helper()
	return shippedBlock(t, tmpl, moduleLocksBlockRe, "MODULE_LOCKS=$(…)")
}

// runShippedBlock runs an extracted assignment under bash in dir and returns the
// value it assigned to varName.
func runShippedBlock(t *testing.T, dir, jq, block, varName string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", block+"\nprintf '%s' \"$"+varName+"\"")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(jq)+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the shipped block failed: %v\n%s", err, out)
	}
	return string(out)
}

// requireJQ resolves jq, skipping locally but failing in CI (ubuntu-latest ships
// jq, so a miss there means the guard went inert).
func requireJQ(t *testing.T) string {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("jq is required to verify the dispatched templates in CI: %v", err)
		}
		t.Skipf("jq not installed: %v", err)
	}
	return jq
}

// TestDriftWorkflowTemplates_ProjectModuleCalls guards the redaction half of the
// module-provenance defect: the plan's `configuration` block carries NO terraform
// sensitivity metadata (before_sensitive/after_sensitive exist only inside
// resource_changes), so anything forwarded from it is unredacted by construction.
// Forwarding the raw `module_calls` subtree therefore shipped every literal module
// argument (`expressions.*.constant_value` — a hardcoded password, an API key) and
// the whole recursive `module` subtree to the callback in cleartext.
//
// Both dispatched pipelines must instead PROJECT each call down to the two fields
// the server actually reads (driftingest.Configuration): `source`, with URL
// userinfo and every non-`ref` query parameter redacted, and `version_constraint`.
// This mirrors moduleCallsPlan() in @4cloudguru/terraform-drift-contract, which
// the suite-profile templates get via the report action/task.
func TestDriftWorkflowTemplates_ProjectModuleCalls(t *testing.T) {
	// The raw forward, verbatim, as it shipped before the fix. Neither template
	// may reintroduce it.
	const rawForward = "module_calls:(.configuration.root_module.module_calls // {})"
	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		if strings.Contains(tmpl, rawForward) {
			t.Errorf("%s template forwards the RAW module_calls subtree; it must project instead", name)
		}
	}

	jq := requireJQ(t)

	// One plan carrying a credential in every place the raw subtree used to leak
	// it: URL userinfo, credential-bearing query parameters, a literal module
	// argument, and the nested module tree's outputs/variable defaults.
	const plan = `{"configuration":{"root_module":{"module_calls":{
	  "vpc":{"source":"git::https://x-access-token:ghp_TOKENSECRET@github.com/org/mod.git?ref=v1.2.3&sshkey=B64PRIVKEY&token=TTT",
	         "version_constraint":"~> 5.0",
	         "expressions":{"db_password":{"constant_value":"hunter2-PLAINTEXT-LEAK"}},
	         "module":{"outputs":{"o":{"value":"NESTED-CONFIG-LEAK"}},
	                   "variables":{"v":{"default":"DEFAULT-LEAK"}}}},
	  "bare":{"source":"acme/vpc/aws","version_constraint":"1.0.0"},
	  "notobj":["a","b"]}}}}`
	secrets := []string{"ghp_TOKENSECRET", "B64PRIVKEY", "TTT", "hunter2-PLAINTEXT-LEAK", "NESTED-CONFIG-LEAK", "DEFAULT-LEAK", "expressions", "constant_value"}

	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "plan.json"), []byte(plan), 0o600); err != nil {
				t.Fatalf("write plan: %v", err)
			}
			got := runShippedBlock(t, dir, jq, moduleCallsBlock(t, tmpl), "MODULE_CALLS")
			for _, s := range secrets {
				if strings.Contains(got, s) {
					t.Errorf("the dispatched payload leaks %q: %s", s, got)
				}
			}
			// Provenance that must survive: the scrubbed source, the ref selector
			// (the one provenance-bearing query parameter) and the constraint.
			for _, want := range []string{
				`"source":"git::https://(redacted)@github.com/org/mod.git?ref=v1.2.3&sshkey=(redacted)&token=(redacted)"`,
				`"version_constraint":"~> 5.0"`,
				`"bare":{"source":"acme/vpc/aws","version_constraint":"1.0.0"}`,
				`"notobj":{}`, // a non-object call projects to an empty object, never a value
			} {
				if !strings.Contains(got, want) {
					t.Errorf("projected payload missing %s\ngot: %s", want, got)
				}
			}
		})
	}
}

// TestDriftWorkflowTemplates_ModuleCallsBounded guards the other half of
// moduleCallsPlan(): at most 100 module calls are emitted, an overflow sets
// module_calls_truncated, and every emitted string is capped at 300 code points
// with the U+2026 marker — so a pathological configuration cannot inflate the
// callback body.
func TestDriftWorkflowTemplates_ModuleCallsBounded(t *testing.T) {
	jq := requireJQ(t)

	calls := make([]string, 0, 150)
	for i := 0; i < 150; i++ {
		calls = append(calls, fmt.Sprintf(`"m%03d":{"source":"ns/n%03d/aws","version_constraint":"1.0.0"}`, i, i))
	}
	// One call whose source and constraint both exceed the 300-code-point cap.
	calls = append(calls, `"long":{"source":"https://example.com/`+strings.Repeat("d", 400)+`","version_constraint":"~> `+strings.Repeat("9", 400)+`"}`)
	plan := `{"configuration":{"root_module":{"module_calls":{` + strings.Join(calls, ",") + `}}}}`

	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "plan.json"), []byte(plan), 0o600); err != nil {
				t.Fatalf("write plan: %v", err)
			}
			out := []byte(runShippedBlock(t, dir, jq, moduleCallsBlock(t, tmpl), "MODULE_CALLS"))
			var got struct {
				Configuration struct {
					RootModule struct {
						ModuleCalls map[string]struct {
							Source            string `json:"source"`
							VersionConstraint string `json:"version_constraint"`
						} `json:"module_calls"`
						Truncated bool `json:"module_calls_truncated"`
					} `json:"root_module"`
				} `json:"configuration"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("payload is not valid JSON: %v\n%s", err, out)
			}
			rm := got.Configuration.RootModule
			if len(rm.ModuleCalls) != 100 {
				t.Errorf("emitted %d module calls, want the 100 cap", len(rm.ModuleCalls))
			}
			if !rm.Truncated {
				t.Error("an over-cap configuration must set module_calls_truncated")
			}
			for name, call := range rm.ModuleCalls {
				for field, v := range map[string]string{"source": call.Source, "version_constraint": call.VersionConstraint} {
					if n := len([]rune(v)); n > 301 {
						t.Errorf("%s.%s is %d code points, want <= 301 (300 + the U+2026 marker)", name, field, n)
					}
				}
			}
		})
	}
}

// TestDriftWorkflowTemplates_ProjectModuleLocks guards the sibling of the
// module_calls defect above, four lines below it in both templates (#376).
//
// .terraform/modules/modules.json is terraform's RESOLVED view of the very same
// module source addresses the configuration block reports, so the credential
// scrubbed out of module_calls — git::https://x-access-token:ghp_…@… — was
// forwarded verbatim one line later by `jq -c .` on the manifest, together with
// Dir (the runner-local checkout path) and any member a later terraform adds.
//
// Both templates must PROJECT the manifest to the provenance the server reads:
// Source + Version (driftingest.ParseModuleLocks) plus Key, which names the call.
// The projection is byte-identical to projectModuleLocks() in
// sethbacon/terraform-drift-report, which the suite-profile templates get via the
// report action/task — the same addresses must redact the same way on both paths.
func TestDriftWorkflowTemplates_ProjectModuleLocks(t *testing.T) {
	// The verbatim forward, as it shipped before the fix. Neither template may
	// reintroduce it.
	const rawForward = "jq -c . .terraform/modules/modules.json"
	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		if strings.Contains(tmpl, rawForward) {
			t.Errorf("%s template forwards the RAW module lockfile; it must project instead", name)
		}
	}

	jq := requireJQ(t)

	// One manifest carrying a credential in every shape a go-getter source can
	// hold it, alongside the entries whose provenance must survive.
	const manifest = `{"Modules":[
	  {"Key":"priv","Source":"git::https://x-access-token:ghp_LOCKFILESECRET@github.com/org/mod.git?ref=v2.0.0&sshkey=B64PRIVKEY&token=TTT","Version":"","Dir":".terraform/modules/priv"},
	  {"Key":"bare","Source":"git::https://ghp_BARETOKEN@github.com/org/m.git","Version":"1.0.0","Dir":"d"},
	  {"Key":"s3","Source":"s3::https://s3.amazonaws.com/b/m.zip?X-Amz-Signature=SIGSECRET","Version":"3.0.0","Dir":"d"},
	  {"Key":"vpc","Source":"acme/vpc/aws","Version":"5.3.0","Dir":"/home/runner/work/RUNNER-PATH-LEAK"},
	  {"Key":"typed","Source":404,"Version":true,"Dir":"d"},
	  ["not","an","object"], "a string", null]}`
	secrets := []string{
		"ghp_LOCKFILESECRET", "B64PRIVKEY", "TTT", "ghp_BARETOKEN", "SIGSECRET",
		"RUNNER-PATH-LEAK", "Dir", // Dir is dropped by construction, key included
	}

	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".terraform", "modules"), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			manifestPath := filepath.Join(dir, ".terraform", "modules", "modules.json")
			if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
			block := moduleLocksBlock(t, tmpl)
			got := runShippedBlock(t, dir, jq, block, "MODULE_LOCKS")
			for _, s := range secrets {
				if strings.Contains(got, s) {
					t.Errorf("the dispatched payload leaks %q: %s", s, got)
				}
			}
			// Provenance that must survive: the scrubbed source, the ref selector
			// (the one provenance-bearing query parameter), Key and Version.
			for _, want := range []string{
				`{"Key":"priv","Source":"git::https://(redacted)@github.com/org/mod.git?ref=v2.0.0&sshkey=(redacted)&token=(redacted)","Version":""}`,
				`{"Key":"bare","Source":"git::https://(redacted)@github.com/org/m.git","Version":"1.0.0"}`,
				`{"Key":"s3","Source":"s3::https://s3.amazonaws.com/b/m.zip?X-Amz-Signature=(redacted)","Version":"3.0.0"}`,
				`{"Key":"vpc","Source":"acme/vpc/aws","Version":"5.3.0"}`,
				`{"Key":"typed"}`, // non-string members are dropped, never coerced
				`{},{},{}`,        // a non-object entry projects to {}, never a value
			} {
				if !strings.Contains(got, want) {
					t.Errorf("projected lockfile missing %s\ngot: %s", want, got)
				}
			}
			// The projection must still satisfy its only consumer: the registry
			// module's locked version has to survive the round trip.
			if locks := driftingest.ParseModuleLocks([]byte(got)); locks["registry.terraform.io|acme/vpc/aws"] != "5.3.0" {
				t.Errorf("ParseModuleLocks lost the locked version after projection: %v", locks)
			}

			// A source longer than the cap is truncated like every other emitted
			// string, so a pathological manifest cannot inflate the callback body.
			long := `{"Modules":[{"Key":"long","Source":"https://example.com/` + strings.Repeat("d", 400) + `","Version":"1.0.0"}]}`
			if err := os.WriteFile(manifestPath, []byte(long), 0o600); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
			var capped struct {
				Modules []struct{ Source string }
			}
			out := runShippedBlock(t, dir, jq, block, "MODULE_LOCKS")
			if err := json.Unmarshal([]byte(out), &capped); err != nil {
				t.Fatalf("payload is not valid JSON: %v\n%s", err, out)
			}
			if n := len([]rune(capped.Modules[0].Source)); n != 301 {
				t.Errorf("Source is %d code points, want 301 (300 + the U+2026 marker)", n)
			}

			// Absent manifest: still exactly `null`, so --argjson stays valid and
			// the server records provenance without locked versions, as before.
			if err := os.Remove(manifestPath); err != nil {
				t.Fatalf("remove manifest: %v", err)
			}
			if out := runShippedBlock(t, dir, jq, block, "MODULE_LOCKS"); out != "null" {
				t.Errorf("absent manifest yielded %q, want null", out)
			}
		})
	}
}

// TestDriftWorkflowTemplates_AreValidYAML is the guard the jq tests do not
// provide. They extract the shell blocks and run them under bash, which proves
// the jq is correct and proves nothing about the document those blocks are
// embedded in — so an edit to a multi-line `run: |` block can ship a template
// that every existing test passes and that no runner can parse. These constants
// are served to users as the file they commit; a YAML break here is an outage in
// someone else's pipeline, discovered by them.
func TestDriftWorkflowTemplates_AreValidYAML(t *testing.T) {
	templates := map[string]string{
		"github drift":          githubDriftWorkflow,
		"azure drift":           azureDriftPipeline,
		"github drift suite":    githubDriftWorkflowSuite,
		"azure drift suite":     azureDriftPipelineSuite,
		"github versionlab":     githubHealthWorkflow,
		"azure versionlab":      azureHealthPipeline,
		"github versionlab ste": githubHealthWorkflowSuite,
		"azure versionlab ste":  azureHealthPipelineSuite,
	}
	for name, tmpl := range templates {
		t.Run(name, func(t *testing.T) {
			var doc map[string]any
			if err := yaml.Unmarshal([]byte(tmpl), &doc); err != nil {
				t.Fatalf("template does not parse as YAML: %v", err)
			}
			// A document that parses to nothing would pass the check above.
			if len(doc) == 0 {
				t.Fatal("template parsed to an empty document")
			}
		})
	}
}
