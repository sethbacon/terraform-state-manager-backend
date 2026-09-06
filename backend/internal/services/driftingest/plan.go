// Package driftingest parses Terraform plan JSON (terraform show -json) pushed
// to the drift ingest endpoint into the same counts + summary shape the
// dispatched CI workflows compute with jq (see internal/api/drift_workflows.go),
// so ingested and dispatched drift render identically. Ported from ogtsm's
// driftingest with the summary adapted to drift_runs' [{address, actions}] form.
//
// The AUTHORITY for these semantics is the canonical TypeScript implementation —
// summarize.ts in @4cloudguru/terraform-drift-contract. Earlier comments here
// cited a Python `drift_summary.py` as the canonical dispatch summarizer; no
// such file exists in any repository of this suite, so it was not something an
// implementation could be diffed against.
//
// The diffing is mechanised by testdata/conformance/vectors.json, a
// byte-identical copy of the contract's corpus that this package, the dispatched
// jq templates and the contract itself all run. See conformance_test.go: a
// semantic change here that the contract has not made reddens the shared digest,
// and vice versa.
package driftingest

import (
	"bytes"
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
	// ResourceDrift is infra drift — hand-edits or other out-of-band changes —
	// as distinct from the unapplied config changes in ResourceChanges. It is
	// optional and purely additive: absent or nil reports zero drift counts
	// and an empty DriftSummary, and never affects ResourceChanges handling,
	// Unparseable, Drifted(), Summary, Unmasked, OmittedEntries or
	// OmittedAttrs. Run through the identical skip/count/mask/bound rules as
	// ResourceChanges via the shared processChanges. Mirrors resource_drift in
	// the canonical @4cloudguru/terraform-drift-contract summarize.ts.
	ResourceDrift []ResourceChange `json:"resource_drift"`
	Configuration Configuration    `json:"configuration"`
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
// nil (rendered as JSON null) for absent/None values; an attribute the plan
// marks sensitive on EITHER mirror has BOTH sides replaced with the literal
// "(sensitive)" before formatting, so a secret never reaches the formatter or
// the stored summary.
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

// MaxEntries and MaxAttrsPerEntry bound the summary. The 300-rune cap in
// fmtVal is per VALUE; without these there is no cap on the number of entries,
// on attrs per entry, or on total bytes, and this Result becomes a stored drift
// record. Measured on the canonical implementation before they existed: 5000
// resources x 50 changed attrs produced a 153.6 MiB body, from a plan an
// attacker can author on a fork PR.
//
// The numbers are the CONTRACT's, declared in
// testdata/conformance/vectors.json under "limits" and asserted against these
// constants by conformance_test.go — so changing a bound means changing the
// corpus, which means changing every implementation.
const (
	MaxEntries       = 500
	MaxAttrsPerEntry = 50
)

// Result carries the counts and summary derived from a plan. Count semantics
// match the CI workflow's jq exactly: a resource counts as added/changed/
// destroyed when its actions CONTAIN create/update/delete respectively, so a
// replacement (delete+create) counts as both added and destroyed.
type Result struct {
	Added     int
	Changed   int
	Destroyed int
	Summary   []SummaryEntry

	// DriftAdded/DriftChanged/DriftDestroyed mirror Added/Changed/Destroyed,
	// computed from Plan.ResourceDrift (infra drift) instead of
	// Plan.ResourceChanges (unapplied config changes) via the identical
	// skip/count/bound rules (see processChanges). Zero when ResourceDrift is
	// absent. Purely additive: never affects Drifted(), Summary, Unmasked,
	// OmittedEntries or OmittedAttrs above. Matches drift_added/drift_changed/
	// drift_destroyed in the canonical contract's Result.
	DriftAdded     int
	DriftChanged   int
	DriftDestroyed int
	// DriftSummary is the ResourceDrift-derived parallel to Summary, rendered
	// through the same rules (skip, attrs, masking, MaxEntries/
	// MaxAttrsPerEntry). Empty, never nil, when ResourceDrift is absent.
	// Matches drift_summary.
	DriftSummary []SummaryEntry

	// Unparseable reports that the document did not have the shape of a plan:
	// no resource_changes array. Without it a truncated `terraform show -json`,
	// the wrong file, or an empty {} produced the identical answer as a verified
	// clean plan — a false negative on the only signal this package exists to
	// produce. Matches `unparseable` in the canonical contract.
	Unparseable bool
	// Unmasked reports that at least one non-skipped change would have emitted
	// attribute values and carried NEITHER sensitivity mirror, so nothing was
	// masked for it. Deliberately shape-based, and therefore slightly over-broad
	// — it is the definition all three implementations can compute identically,
	// and over-warning is the right direction for a redaction signal. A
	// present-but-false mirror IS metadata and does not set it.
	Unmasked bool
	// OmittedEntries counts summary rows dropped by MaxEntries. The COUNTS above
	// still include them, so Drifted() stays truthful when the summary is capped.
	OmittedEntries int
	// OmittedAttrs counts changed attributes dropped by MaxAttrsPerEntry.
	OmittedAttrs int
}

// Truncated reports that a bound was reached and the summary is not the whole
// story.
func (r *Result) Truncated() bool { return r.OmittedEntries > 0 || r.OmittedAttrs > 0 }

// Drifted reports whether the plan planned any add/change/destroy. Matches the
// canonical contract (summarize() in @4cloudguru/terraform-drift-contract): a
// pure replacement has Changed==0 but Added+Destroyed>0, so it is still drift; a
// read-only refresh is not.
func (r *Result) Drifted() bool {
	return r.Added+r.Changed+r.Destroyed > 0
}

// processedChanges is the count/summary/masking output of running one array of
// resource changes through the shared skip/count/mask/bound rules — mirrors
// ProcessedChanges in the canonical @4cloudguru/terraform-drift-contract
// summarize.ts.
type processedChanges struct {
	Added          int
	Changed        int
	Destroyed      int
	Summary        []SummaryEntry
	Unmasked       bool
	OmittedEntries int
	OmittedAttrs   int
}

// processChanges is the per-item loop shared by Plan.ResourceChanges and
// Plan.ResourceDrift: count, skip, mask and bound. Extracted so the two paths
// run through IDENTICAL logic rather than a parallel copy — a second copy is
// exactly how the two would drift apart, which is the bug drift counting
// exists to avoid (mirrors the extraction of processChanges() in the
// canonical summarize.ts, made for the same reason).
func processChanges(changes []ResourceChange, maxEntries, maxAttrsPerEntry int) processedChanges {
	res := processedChanges{Summary: []SummaryEntry{}}
	for _, rc := range changes {
		actions := rc.Change.Actions
		if len(actions) == 1 && (actions[0] == "no-op" || actions[0] == "read") {
			continue
		}
		// Counted BEFORE the entry cap, so a capped summary still reports drift
		// truthfully: the counts are the security signal, the rows are detail.
		if hasAction(actions, "create") {
			res.Added++
		}
		if hasAction(actions, "update") {
			res.Changed++
		}
		if hasAction(actions, "delete") {
			res.Destroyed++
		}

		attrs, omitted, inPlace := changedAttrs(rc.Change, maxAttrsPerEntry)
		// A property of the PLAN, not of how much of it fit in the summary, so it
		// is evaluated for capped entries too.
		if inPlace && canon(rc.Change.BeforeSensitive) == "null" && canon(rc.Change.AfterSensitive) == "null" {
			res.Unmasked = true
		}
		if len(res.Summary) >= maxEntries {
			res.OmittedEntries++
			continue
		}
		res.OmittedAttrs += omitted

		entry := SummaryEntry{Address: rc.Address, Actions: actions}
		if len(attrs) > 0 {
			entry.Attrs = attrs
		}
		res.Summary = append(res.Summary, entry)
	}
	return res
}

// Summarize classifies each resource change, reconciled with the canonical
// contract (summarize.ts in @4cloudguru/terraform-drift-contract, plus its test
// vectors — see the package comment): resource changes whose actions are
// exactly ["no-op"] or ["read"] are skipped entirely; for in-place updates and
// replacements (before and after both JSON objects) the per-attribute diff is
// captured with sensitive masking. Counts are replace-aware and not mutually
// exclusive (a replacement counts as both added and destroyed).
//
// Plan.ResourceDrift runs through the identical rules (via processChanges) to
// produce the parallel DriftAdded/DriftChanged/DriftDestroyed/DriftSummary,
// but — matching the canonical contract exactly — its Unmasked,
// OmittedEntries and OmittedAttrs are computed and then discarded: those three
// fields, along with Drifted() and Summary, describe Plan.ResourceChanges
// only. This is deliberate, not an oversight: resource_drift is purely
// additive, so it must not be able to flip Drifted() or Truncated() for a
// plan whose ResourceChanges alone would report clean.
func Summarize(plan *Plan) *Result {
	res := &Result{Summary: []SummaryEntry{}, DriftSummary: []SummaryEntry{}}
	if plan == nil {
		res.Unparseable = true
		return res
	}
	// nil vs empty slice is the whole signal here and is load-bearing:
	// encoding/json leaves the field nil for an absent key and for an explicit
	// null, and allocates an empty non-nil slice for []. So a genuinely clean
	// plan is distinguishable from a document that is not a plan at all —
	// which is what Unparseable reports. Pinned by conformance vectors
	// shape/not-a-plan-document and clean/empty-resource-changes; do not
	// "simplify" this to len() == 0.
	res.Unparseable = plan.ResourceChanges == nil

	primary := processChanges(plan.ResourceChanges, MaxEntries, MaxAttrsPerEntry)
	res.Added = primary.Added
	res.Changed = primary.Changed
	res.Destroyed = primary.Destroyed
	res.Summary = primary.Summary
	res.Unmasked = primary.Unmasked
	res.OmittedEntries = primary.OmittedEntries
	res.OmittedAttrs = primary.OmittedAttrs

	drift := processChanges(plan.ResourceDrift, MaxEntries, MaxAttrsPerEntry)
	res.DriftAdded = drift.Added
	res.DriftChanged = drift.Changed
	res.DriftDestroyed = drift.Destroyed
	res.DriftSummary = drift.Summary

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
// before and after, plus the number dropped by maxAttrs and whether this was the
// in-place case at all (which the caller needs for the Unmasked signal). Only
// when both sides are JSON objects (the in-place update / replace case).
//
// An attribute is masked to "(sensitive)" on BOTH sides when EITHER
// before_sensitive or after_sensitive marks it; otherwise both sides are
// formatted with fmtVal. Mirrors the loop in the canonical
// @4cloudguru/terraform-drift-contract summarize().
func changedAttrs(ch Change, maxAttrs int) (attrs []AttrChange, omitted int, inPlace bool) {
	before, bok := asObject(ch.Before)
	after, aok := asObject(ch.After)
	if !bok || !aok {
		return nil, 0, false
	}
	inPlace = true
	attrs = []AttrChange{}
	for _, k := range sortedUnion(before, after) {
		if jsonEqual(before[k], after[k]) {
			continue
		}
		// Past the cap the key is still KNOWN to have changed — the comparison
		// above has already run — so it is counted, not concealed. Only the
		// formatting is skipped, which is the expensive half.
		if len(attrs) >= maxAttrs {
			omitted++
			continue
		}
		// Union, not per-side: terraform applies a config-derived mark (a
		// `sensitive = true` variable, sensitive(), a sensitive module output) to
		// the PLANNED value only — it is never persisted to state — so a
		// credential routinely arrives marked on exactly one side. Masking each
		// side against its own mirror emitted the other side in cleartext (the
		// `~ user_data = "old-plaintext" -> (sensitive value)` shape). Over-masking
		// a symmetric pair costs nothing: both sides already render "(sensitive)".
		//
		// When NEITHER mirror is present the value is emitted unmasked. That is
		// deliberate and fail-open (see the contract's SECURITY.md): plans without
		// sensitivity metadata are common, and masking them would mask every
		// attribute of every such plan.
		sensitive := isSens(ch.BeforeSensitive, k) || isSens(ch.AfterSensitive, k)
		attrs = append(attrs, AttrChange{
			Name:   k,
			Before: maskOrFmt(sensitive, before[k]),
			After:  maskOrFmt(sensitive, after[k]),
		})
	}
	if len(attrs) == 0 {
		return nil, omitted, true
	}
	return attrs, omitted, true
}

// maskOrFmt yields the literal "(sensitive)" when the attribute is masked (so a
// secret never reaches the formatter), otherwise the formatted value.
func maskOrFmt(sensitive bool, val json.RawMessage) *string {
	if sensitive {
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

// marshalCanon is the ONE serializer this package emits and compares with, and
// the byte form it produces is the contract's — `stableStringify` in
// @4cloudguru/terraform-drift-contract, asserted vector by vector against
// testdata/conformance/vectors.json.
//
// encoding/json HTML-escapes `<`, `>` and `&` by default, and those three
// characters appear in every IAM policy document, user_data script and
// connection string — so `{"policy":"a<b&c>d"}` was stored here as
// `{"policy":"a<b&c>d"}` and by the report action as the raw
// text, for the identical plan. SetEscapeHTML(false) is the only knob needed:
// non-ASCII is already emitted raw, map keys are already sorted as UTF-8 bytes
// (which is code-point order), and the encoder's unconditional U+2028/U+2029
// escaping is the one axis where the contract moved to meet THIS side.
//
// Encoder.Encode appends a newline; the canonical form has none.
func marshalCanon(v interface{}) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// fmtVal is the Go port of the contract's `fmt()`: JSON null/absent → nil;
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
	out := truncate(marshalCanon(v))
	return &out
}

func truncate(s string) string {
	if utf8.RuneCountInString(s) <= 300 {
		return s
	}
	return string([]rune(s)[:300]) + "…"
}

// isSens is the Go port of the contract's `isSens()`: when the sensitivity map
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
	// The SAME serializer the value is emitted with: if equality canonicalised
	// `<` one way and fmtVal emitted it another, a change could be reported with
	// a form the comparison never saw.
	return marshalCanon(v)
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
