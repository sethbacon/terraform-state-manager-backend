package driftingest

import (
	"encoding/json"
	"testing"
)

func TestCanonicalHost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "reg.example.com", "reg.example.com"},
		{"uppercase", "REG.Example.COM", "reg.example.com"},
		{"trailing dot", "reg.example.com.", "reg.example.com"},
		{"default https port stripped", "reg.example.com:443", "reg.example.com"},
		{"default http port stripped", "reg.example.com:80", "reg.example.com"},
		{"non-default port preserved", "reg.example.com:8443", "reg.example.com:8443"},
		{"scheme stripped", "https://reg.example.com/", "reg.example.com"},
		{"scheme + default port", "https://REG.Example.com.:443/v1/modules/", "reg.example.com"},
		{"public registry idempotent", "registry.terraform.io", "registry.terraform.io"},
		{"ipv4 with port", "10.0.0.5:8443", "10.0.0.5:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalHost(tc.in); got != tc.want {
				t.Errorf("CanonicalHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestModuleRefs_CanonicalizesHost proves capture folds host variants so the
// stored registry_host matches the registry's emitted (also-canonical) join key.
func TestModuleRefs_CanonicalizesHost(t *testing.T) {
	// Host-prefixed source with uppercase + an explicit default port: must be
	// stored as the bare lowercase host.
	planJSON := `{
		"configuration": {
			"root_module": {
				"module_calls": {
					"vpc": {"source": "REG.Example.com:443/myorg/vpc/aws", "version_constraint": "1.0.0"}
				}
			}
		}
	}`
	var plan Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var found bool
	for _, r := range ModuleRefs(&plan) {
		if r.ModuleSource == "myorg/vpc/aws" {
			found = true
			if r.RegistryHost != "reg.example.com" {
				t.Errorf("RegistryHost = %q, want canonical %q", r.RegistryHost, "reg.example.com")
			}
		}
	}
	if !found {
		t.Fatalf("expected a captured ref for myorg/vpc/aws")
	}
}
