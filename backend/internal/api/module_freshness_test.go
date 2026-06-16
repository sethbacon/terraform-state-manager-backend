package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sethbacon/terraform-suite-identity/identity/suite"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

func freshnessVerPtr(s string) *string { return &s }

// stubRegistry serves the registry versions endpoint for two modules and counts
// hits so we can assert constraint-only modules trigger NO outbound call.
func stubRegistry(t *testing.T, hits *map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*hits)[r.URL.Path]++
		switch r.URL.Path {
		case "/v1/modules/acme/vpc/aws/versions":
			// 5.7.1 is the latest NON-deprecated; 9.9.9 is deprecated (must be ignored).
			_, _ = w.Write([]byte(`{"modules":[{"versions":[
				{"version":"5.3.0"},{"version":"v5.7.1"},{"version":"9.9.9","deprecated":true}]}]}`))
		case "/v1/modules/acme/db/aws/versions":
			w.WriteHeader(http.StatusNotFound) // -> unknown
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

func TestComputeFreshness_ActiveSibling(t *testing.T) {
	hits := map[string]int{}
	reg := stubRegistry(t, &hits)
	defer reg.Close()
	host := suite.CanonicalHost(reg.URL)

	refs := []repositories.StateModuleRef{
		{ModuleSource: "acme/vpc/aws", RegistryHost: host, ModuleVersion: freshnessVerPtr("5.3.0")},               // behind 5.7.1
		{ModuleSource: "acme/vpc/aws", RegistryHost: host, ModuleVersion: freshnessVerPtr("5.7.1")},               // up_to_date
		{ModuleSource: "acme/cons/aws", RegistryHost: host, ModuleVersion: nil},                                   // constraint_only (no call)
		{ModuleSource: "acme/db/aws", RegistryHost: host, ModuleVersion: freshnessVerPtr("1.0.0")},                // unknown (404)
		{ModuleSource: "x/y/aws", RegistryHost: "registry.terraform.io", ModuleVersion: freshnessVerPtr("1.0.0")}, // no_registry (host)
	}

	got := computeFreshness(context.Background(), &http.Client{}, reg.URL, host, refs)
	if len(got) != len(refs) {
		t.Fatalf("got %d verdicts, want %d", len(got), len(refs))
	}

	want := []struct {
		status string
		latest string // "" => nil
	}{
		{"behind", "v5.7.1"},
		{"up_to_date", "v5.7.1"},
		{"constraint_only", ""},
		{"unknown", ""},
		{"no_registry", ""},
	}
	for i, w := range want {
		if got[i].Status != w.status {
			t.Errorf("ref %d: status = %q, want %q", i, got[i].Status, w.status)
		}
		if w.latest == "" {
			if got[i].Latest != nil {
				t.Errorf("ref %d: latest = %v, want nil", i, *got[i].Latest)
			}
		} else if got[i].Latest == nil || *got[i].Latest != w.latest {
			t.Errorf("ref %d: latest = %v, want %q (deprecated 9.9.9 must be ignored)", i, got[i].Latest, w.latest)
		}
	}

	// constraint_only must make ZERO registry calls; the two vpc refs share one
	// cached lookup (fetched once).
	if hits["/v1/modules/acme/cons/aws/versions"] != 0 {
		t.Errorf("constraint_only module triggered a registry call (want 0)")
	}
	if hits["/v1/modules/acme/vpc/aws/versions"] != 1 {
		t.Errorf("vpc fetched %d times, want 1 (per-request cache)", hits["/v1/modules/acme/vpc/aws/versions"])
	}
}

func TestComputeFreshness_Standalone(t *testing.T) {
	refs := []repositories.StateModuleRef{
		{ModuleSource: "acme/vpc/aws", RegistryHost: "app.terraform.io", ModuleVersion: freshnessVerPtr("5.3.0")},
		{ModuleSource: "acme/db/aws", RegistryHost: "app.terraform.io", ModuleVersion: nil},
	}
	// No active sibling: siblingURL/host empty.
	got := computeFreshness(context.Background(), &http.Client{}, "", "", refs)
	for i, mf := range got {
		if mf.Status != "no_registry" {
			t.Errorf("ref %d: standalone status = %q, want no_registry", i, mf.Status)
		}
		if mf.Latest != nil {
			t.Errorf("ref %d: standalone latest must be nil", i)
		}
	}
	// current is still the locked version (informational), even when no_registry.
	if got[0].Current == nil || *got[0].Current != "5.3.0" {
		t.Errorf("current should carry the locked version even when no_registry")
	}
}
