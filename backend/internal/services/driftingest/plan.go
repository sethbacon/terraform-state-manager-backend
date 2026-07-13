// Package driftingest parses Terraform plan JSON (terraform show -json) pushed
// to the drift ingest endpoint into the same counts + summary shape the
// dispatched CI workflows compute with jq (see internal/api/drift_workflows.go),
// so ingested and dispatched drift render identically. Ported from ogtsm's
// driftingest with the summary adapted to drift_runs' [{address, actions}] form.
package driftingest

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

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
// ["create"], ["update"], ["delete"], or ["delete","create"] for a replacement,
// plus the before/after values (and their sensitivity masks) used to derive the
// per-attribute diff. before/after are raw so we only parse them when both are
// JSON objects (in-place updates/replaces).
type Change struct {
	Actions         []string        `json:"actions"`
	Before          json.RawMessage `json:"before"`
	After           json.RawMessage `json:"after"`
	BeforeSensitive json.RawMessage `json:"before_sensitive"`
	AfterSensitive  json.RawMessage `json:"after_sensitive"`
}

// AttrChange is one changed top-level attribute of a resource. Before/After are
// nil (rendered as JSON null) for absent/None values; a value the plan marks
// sensitive is replaced with the literal "(sensitive)" before formatting, so a
// secret never reaches the formatter or the stored summary.
type AttrChange struct {
	Name   string  `json:"name"`
	Before *string `json:"before"`
	After  *string `json:"after"`
}

// SummaryEntry matches the rows of drift_runs.summary so the frontend renders
// both origins (ingested + dispatched) uniformly. Attrs is omitted unless the
// change is an in-place update/replace with at least one differing key.
type SummaryEntry struct {
	Address string       `json:"address"`
	Actions []string     `json:"actions"`
	Attrs   []AttrChange `json:"attrs,omitempty"`
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

// Drifted reports whether the plan planned any add/change/destroy. Matches the
// dispatch summarizer (drift_summary.py): a pure replacement has Changed==0 but
// Added+Destroyed>0, so it is still drift; a read-only refresh is not.
func (r *Result) Drifted() bool {
	return r.Added+r.Changed+r.Destroyed > 0
}

// Summarize classifies each resource change, reconciled with the authoritative
// dispatch summarizer (drift_summary.py): resource changes whose actions are
// exactly ["no-op"] or ["read"] are skipped entirely; for in-place updates and
// replacements (before and after both JSON objects) the per-attribute diff is
// captured with sensitive masking. Counts are replace-aware and not mutually
// exclusive (a replacement counts as both added and destroyed).
func Summarize(plan *Plan) *Result {
	res := &Result{Summary: []SummaryEntry{}}
	if plan == nil {
		return res
	}
	for _, rc := range plan.ResourceChanges {
		actions := rc.Change.Actions
		if len(actions) == 1 && (actions[0] == "no-op" || actions[0] == "read") {
			continue
		}
		entry := SummaryEntry{Address: rc.Address, Actions: actions}
		if attrs := changedAttrs(rc.Change); len(attrs) > 0 {
			entry.Attrs = attrs
		}
		res.Summary = append(res.Summary, entry)
		if hasAction(actions, "create") {
			res.Added++
		}
		if hasAction(actions, "update") {
			res.Changed++
		}
		if hasAction(actions, "delete") {
			res.Destroyed++
		}
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

// changedAttrs returns the top-level attributes whose value differs between
// before and after, but only when both are JSON objects (the in-place update /
// replace case). Each value is masked to "(sensitive)" when before_sensitive /
// after_sensitive marks it, otherwise formatted with fmtVal. Mirrors the loop
// in drift_summary.py.
func changedAttrs(ch Change) []AttrChange {
	before, bok := asObject(ch.Before)
	after, aok := asObject(ch.After)
	if !bok || !aok {
		return nil
	}
	attrs := []AttrChange{}
	for _, k := range sortedUnion(before, after) {
		if jsonEqual(before[k], after[k]) {
			continue
		}
		attrs = append(attrs, AttrChange{
			Name:   k,
			Before: maskOrFmt(ch.BeforeSensitive, k, before[k]),
			After:  maskOrFmt(ch.AfterSensitive, k, after[k]),
		})
	}
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}

func maskOrFmt(sens json.RawMessage, key string, val json.RawMessage) *string {
	if isSens(sens, key) {
		s := "(sensitive)"
		return &s
	}
	return fmtVal(val)
}

// asObject reports whether raw is a JSON object and, if so, returns its members.
// JSON null (a pure create's before, a pure delete's after) is NOT an object —
// unmarshaling null into a map succeeds with a nil map, so guard it explicitly.
func asObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

func sortedUnion(a, b map[string]json.RawMessage) []string {
	seen := map[string]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fmtVal is the Go port of drift_summary.py `fmt`: JSON null/absent → nil;
// a JSON string passes through raw (unquoted); anything else is compact
// canonical JSON (Go sorts map keys), truncated past 300 runes with U+2026.
func fmtVal(raw json.RawMessage) *string {
	if canon(raw) == "null" {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		s := truncate(string(raw))
		return &s
	}
	if s, ok := v.(string); ok {
		out := truncate(s)
		return &out
	}
	b, _ := json.Marshal(v)
	out := truncate(string(b))
	return &out
}

func truncate(s string) string {
	if utf8.RuneCountInString(s) <= 300 {
		return s
	}
	return string([]rune(s)[:300]) + "…"
}

// isSens is the Go port of drift_summary.py `is_sens`: when the sensitivity map
// is not a JSON object it is the whole-value flag (truthy → mask); otherwise the
// key is masked when its entry is true or a non-empty nested object/array.
func isSens(sens json.RawMessage, key string) bool {
	if len(sens) == 0 {
		return false
	}
	var v interface{}
	if err := json.Unmarshal(sens, &v); err != nil {
		return false
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return pyTruthy(v)
	}
	sv, exists := m[key]
	if !exists {
		return false
	}
	if b, ok := sv.(bool); ok {
		return b
	}
	switch t := sv.(type) {
	case map[string]interface{}:
		return len(t) > 0
	case []interface{}:
		return len(t) > 0
	default:
		return false
	}
}

// pyTruthy mirrors Python bool(): empty string/array/object and zero/false/None
// are falsey.
func pyTruthy(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []interface{}:
		return len(t) > 0
	case map[string]interface{}:
		return len(t) > 0
	default:
		return false
	}
}

// jsonEqual compares two raw JSON values structurally (key order independent),
// treating absent and explicit null alike — matching Python's `==` on the
// decoded values.
func jsonEqual(a, b json.RawMessage) bool {
	return canon(a) == canon(b)
}

func canon(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "null"
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ModuleRef is one captured registry-module dependency of a state: the module's
// source address (ns/name/provider), the registry host it resolves to, and (when
// known) a locked version. ModuleVersion is nil unless a module lockfile
// (.terraform/modules/modules.json) was ingested alongside the plan — the plan's
// configuration carries only a version constraint, which is not a locked version.
type ModuleRef struct {
	ModuleSource  string
	RegistryHost  string
	ModuleVersion *string
}

// ModuleLocks maps a registry module's canonical (host, source) identity to its
// resolved/locked version, parsed from a Terraform module manifest
// (.terraform/modules/modules.json). It is the only source of a *locked* module
// version available to the ingest path; pass nil when no manifest was uploaded.
type ModuleLocks map[string]string

// lockKey is the join key shared by ParseModuleLocks and ModuleRefs: both sides
// run their raw source through registryModuleAddress first, so the lookup is
// exact regardless of //subdir selectors, host casing, or default ports.
func lockKey(host, source string) string { return host + "|" + source }

// ParseModuleLocks reads a `.terraform/modules/modules.json` manifest into a
// (host, source) → version map. Only registry modules with a resolved Version
// are included; the root module, local, and VCS entries (no Version, or a
// non-registry source) are skipped. Invalid JSON yields nil (the caller then
// captures provenance without locked versions, exactly as before lockfiles).
func ParseModuleLocks(raw []byte) ModuleLocks {
	if len(raw) == 0 {
		return nil
	}
	var doc struct {
		Modules []struct {
			Source  string `json:"Source"`
			Version string `json:"Version"`
		} `json:"Modules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	locks := ModuleLocks{}
	for _, m := range doc.Modules {
		if m.Version == "" {
			continue
		}
		host, source, ok := registryModuleAddress(m.Source)
		if !ok {
			continue
		}
		locks[lockKey(host, source)] = m.Version
	}
	if len(locks) == 0 {
		return nil
	}
	return locks
}

// ModuleRefs extracts registry-module provenance from a plan's top-level module
// calls. Only registry-style sources are captured (bare "ns/name/provider" → the
// public registry, or "host/ns/name/provider" → that host); local ("./", "../")
// and VCS (git::, github.com/...) sources are skipped because they have no
// registry host to join on. When locks is non-nil, each ref's resolved version
// is filled in from the matching module-manifest entry.
func ModuleRefs(plan *Plan, locks ModuleLocks) []ModuleRef {
	if plan == nil {
		return nil
	}
	refs := []ModuleRef{}
	for _, call := range plan.Configuration.RootModule.ModuleCalls {
		host, source, ok := registryModuleAddress(call.Source)
		if !ok {
			continue
		}
		ref := ModuleRef{ModuleSource: source, RegistryHost: host}
		if locks != nil {
			if v := locks[lockKey(host, source)]; v != "" {
				ref.ModuleVersion = &v
			}
		}
		refs = append(refs, ref)
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
		// regardless of case / default port / trailing dot. CanonicalHost fails
		// closed (returns "") on userinfo-bearing or otherwise-unrecoverable host
		// input — treat that the same as an unparseable host rather than letting
		// a malformed source string produce ok=true with an empty RegistryHost.
		host := suite.CanonicalHost(parts[0])
		if host == "" {
			return "", "", false
		}
		return host, strings.Join(parts[1:], "/"), true
	default:
		return "", "", false
	}
}
