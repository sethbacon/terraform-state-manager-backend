// Package driftingest parses Terraform plan JSON (terraform show -json) pushed
// to the drift ingest endpoint into the same counts + summary shape the
// dispatched CI workflows compute with jq (see internal/api/drift_workflows.go),
// so ingested and dispatched drift render identically. Ported from ogtsm's
// driftingest with the summary adapted to drift_runs' [{address, actions}] form.
package driftingest

import (
	"strings"

	"github.com/sethbacon/terraform-suite-identity/identity/suite"
)

// Plan is the subset of a `terraform show -json` document the ingest path
// needs; all other top-level keys are ignored.
type Plan struct {
	ResourceChanges []ResourceChange `json:"resource_changes"`
	Configuration   Configuration    `json:"configuration"`
}

// Configuration is the subset of the plan's `configuration` block used to
// capture module provenance: the source address + version constraint of each
// top-level module call. Nested/transitive module calls are not captured.
type Configuration struct {
	RootModule struct {
		ModuleCalls map[string]struct {
			Source            string `json:"source"`
			VersionConstraint string `json:"version_constraint"`
		} `json:"module_calls"`
	} `json:"root_module"`
}

// ResourceChange mirrors one entry of the plan's resource_changes array.
type ResourceChange struct {
	Address string `json:"address"`
	Change  Change `json:"change"`
}

// Change holds the planned actions for a single resource: ["no-op"],
// ["create"], ["update"], ["delete"], or ["delete","create"] for a replacement.
type Change struct {
	Actions []string `json:"actions"`
}

// SummaryEntry matches the rows of drift_runs.summary so the frontend renders
// both origins uniformly.
type SummaryEntry struct {
	Address string   `json:"address"`
	Actions []string `json:"actions"`
}

// Result carries the counts and summary derived from a plan. Count semantics
// match the CI workflow's jq exactly: a resource counts as added/changed/
// destroyed when its actions CONTAIN create/update/delete respectively, so a
// replacement (delete+create) counts as both added and destroyed.
type Result struct {
	Added     int
	Changed   int
	Destroyed int
	Summary   []SummaryEntry
}

// Drifted reports whether the plan contained any non-no-op resource change.
func (r *Result) Drifted() bool {
	return len(r.Summary) > 0
}

// Summarize classifies each resource change. Entries whose actions are exactly
// ["no-op"] are excluded from the summary (the jq filter `!= ["no-op"]`);
// read-only refreshes (["read"]) appear in the summary but count toward
// nothing, again matching the workflow.
func Summarize(plan *Plan) *Result {
	res := &Result{Summary: []SummaryEntry{}}
	if plan == nil {
		return res
	}
	for _, rc := range plan.ResourceChanges {
		if hasAction(rc.Change.Actions, "create") {
			res.Added++
		}
		if hasAction(rc.Change.Actions, "update") {
			res.Changed++
		}
		if hasAction(rc.Change.Actions, "delete") {
			res.Destroyed++
		}
		if len(rc.Change.Actions) == 1 && rc.Change.Actions[0] == "no-op" {
			continue
		}
		res.Summary = append(res.Summary, SummaryEntry{Address: rc.Address, Actions: rc.Change.Actions})
	}
	return res
}

func hasAction(actions []string, action string) bool {
	for _, a := range actions {
		if a == action {
			return true
		}
	}
	return false
}

// ModuleRef is one captured registry-module dependency of a state: the module's
// source address (ns/name/provider), the registry host it resolves to, and (when
// known) a locked version. ModuleVersion is always nil today — only the plan's
// version constraint is available without a lockfile, and a constraint is not a
// locked version.
type ModuleRef struct {
	ModuleSource  string
	RegistryHost  string
	ModuleVersion *string
}

// ModuleRefs extracts registry-module provenance from a plan's top-level module
// calls. Only registry-style sources are captured (bare "ns/name/provider" → the
// public registry, or "host/ns/name/provider" → that host); local ("./", "../")
// and VCS (git::, github.com/...) sources are skipped because they have no
// registry host to join on.
func ModuleRefs(plan *Plan) []ModuleRef {
	if plan == nil {
		return nil
	}
	refs := []ModuleRef{}
	for _, call := range plan.Configuration.RootModule.ModuleCalls {
		host, source, ok := registryModuleAddress(call.Source)
		if !ok {
			continue
		}
		refs = append(refs, ModuleRef{ModuleSource: source, RegistryHost: host})
	}
	return refs
}

// publicRegistryHost is the implied host for a bare "ns/name/provider" address.
const publicRegistryHost = "registry.terraform.io"

// registryModuleAddress parses a Terraform module source into its registry host
// and "ns/name/provider" source, reporting ok=false for non-registry (local or
// VCS) sources. Conservative by design (the honesty guard): anything that does
// not look exactly like a registry address is skipped.
func registryModuleAddress(src string) (host, source string, ok bool) {
	if src == "" ||
		strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || strings.HasPrefix(src, "/") ||
		strings.Contains(src, "::") || strings.Contains(src, "?") || strings.HasPrefix(src, "git@") {
		return "", "", false
	}
	// Drop a "//subdir" submodule selector; the parent module is the dependency.
	if i := strings.Index(src, "//"); i >= 0 {
		src = src[:i]
	}
	parts := strings.Split(src, "/")
	switch len(parts) {
	case 3: // ns/name/provider → public registry; first part must NOT look like a host
		if strings.ContainsAny(parts[0], ".:") {
			return "", "", false // e.g. github.com/org/repo is VCS, not a registry module
		}
		// publicRegistryHost is already canonical; routed through for symmetry.
		return suite.CanonicalHost(publicRegistryHost), src, true
	case 4: // host/ns/name/provider → host-prefixed registry; first part must look like a host
		if !strings.ContainsAny(parts[0], ".:") {
			return "", "", false
		}
		// Canonicalize so the stored host matches the registry's emitted join key
		// regardless of case / default port / trailing dot.
		return suite.CanonicalHost(parts[0]), strings.Join(parts[1:], "/"), true
	default:
		return "", "", false
	}
}
