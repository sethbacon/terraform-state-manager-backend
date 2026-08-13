package driftingest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSummarize_CountsMatchDispatchSummarizer(t *testing.T) {
	// Semantics contract (reconciled with drift_summary.py): counts are action
	// MEMBERSHIP (a replacement counts as added AND destroyed); summary excludes
	// changes whose actions are exactly ["no-op"] OR ["read"].
	planJSON := `{
		"format_version": "1.2",
		"resource_changes": [
			{"address": "aws_instance.new",      "change": {"actions": ["create"]}},
			{"address": "aws_instance.tweak",    "change": {"actions": ["update"]}},
			{"address": "aws_instance.gone",     "change": {"actions": ["delete"]}},
			{"address": "aws_instance.replaced", "change": {"actions": ["delete", "create"]}},
			{"address": "aws_instance.same",     "change": {"actions": ["no-op"]}},
			{"address": "data.aws_ami.x",        "change": {"actions": ["read"]}}
		]
	}`
	var plan Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	res := Summarize(&plan)
	if res.Added != 2 || res.Changed != 1 || res.Destroyed != 2 {
		t.Errorf("counts = +%d ~%d -%d, want +2 ~1 -2", res.Added, res.Changed, res.Destroyed)
	}
	if len(res.Summary) != 4 { // everything except the no-op AND the read
		t.Errorf("summary entries = %d, want 4 (%+v)", len(res.Summary), res.Summary)
	}
	for _, e := range res.Summary {
		if e.Address == "data.aws_ami.x" || e.Address == "aws_instance.same" {
			t.Errorf("read/no-op must be skipped, found %s", e.Address)
		}
		if e.Address == "aws_instance.replaced" && len(e.Actions) != 2 {
			t.Errorf("replacement actions = %v", e.Actions)
		}
	}
	if !res.Drifted() {
		t.Error("plan with changes must report drifted")
	}
}

func TestSummarize_DriftedIsCountBased(t *testing.T) {
	// A pure replacement has Changed==0 but is still drift (Added+Destroyed>0).
	res := Summarize(&Plan{ResourceChanges: []ResourceChange{
		{Address: "aws_instance.r", Change: Change{Actions: []string{"delete", "create"}}},
	}})
	if res.Changed != 0 || !res.Drifted() {
		t.Errorf("replace must be drifted with Changed==0: %+v", res)
	}
}

func TestSummarize_NoOpReadAndNilPlans(t *testing.T) {
	res := Summarize(&Plan{ResourceChanges: []ResourceChange{
		{Address: "aws_instance.same", Change: Change{Actions: []string{"no-op"}}},
	}})
	if res.Drifted() || len(res.Summary) != 0 {
		t.Errorf("no-op plan must be clean: %+v", res)
	}

	if res := Summarize(nil); res.Drifted() || res.Summary == nil {
		t.Errorf("nil plan must yield an empty, clean result: %+v", res)
	}

	// A read-only refresh is now skipped entirely (matching drift_summary.py):
	// empty summary, not drifted.
	res = Summarize(&Plan{ResourceChanges: []ResourceChange{
		{Address: "data.aws_ami.x", Change: Change{Actions: []string{"read"}}},
	}})
	if res.Drifted() || len(res.Summary) != 0 {
		t.Errorf("read-only must be skipped entirely: %+v", res)
	}
}

func TestSummarize_AttrsAndSensitiveMasking(t *testing.T) {
	// In-place update: a normal attribute is formatted, a sensitive one is
	// masked, and an unchanged nested object is omitted (deep equality).
	planJSON := `{
		"resource_changes": [
			{"address": "aws_db.x", "change": {
				"actions": ["update"],
				"before": {"size": "small", "tags": {"env": "dev"}, "password": "old"},
				"after":  {"size": "large", "tags": {"env": "dev"}, "password": "new"},
				"before_sensitive": {"password": true},
				"after_sensitive":  {"password": true}
			}},
			{"address": "aws_x.created", "change": {"actions": ["create"], "before": null, "after": {"k": "v"}}}
		]
	}`
	var plan Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	res := Summarize(&plan)

	var upd *SummaryEntry
	for i := range res.Summary {
		if res.Summary[i].Address == "aws_db.x" {
			upd = &res.Summary[i]
		}
		if res.Summary[i].Address == "aws_x.created" && res.Summary[i].Attrs != nil {
			t.Error("pure create (before=null) must have no attrs")
		}
	}
	if upd == nil {
		t.Fatal("missing update entry")
	}
	if len(upd.Attrs) != 2 { // size + password; tags unchanged → omitted
		t.Fatalf("attrs = %+v, want [size password]", upd.Attrs)
	}
	got := map[string][2]*string{}
	for _, a := range upd.Attrs {
		got[a.Name] = [2]*string{a.Before, a.After}
	}
	if got["size"][0] == nil || *got["size"][0] != "small" || *got["size"][1] != "large" {
		t.Errorf("size attr = %v", got["size"])
	}
	if got["password"][0] == nil || *got["password"][0] != "(sensitive)" || *got["password"][1] != "(sensitive)" {
		t.Errorf("password must be masked both sides: %v", got["password"])
	}
	if _, ok := got["tags"]; ok {
		t.Error("unchanged nested object must be omitted from attrs")
	}
}

func TestFmtVal_StringPassthroughTruncationAndNull(t *testing.T) {
	if got := fmtVal(json.RawMessage(`"hello"`)); got == nil || *got != "hello" {
		t.Errorf("string passthrough: %v", got)
	}
	if got := fmtVal(json.RawMessage(`null`)); got != nil {
		t.Errorf("null must be nil: %v", got)
	}
	if got := fmtVal(json.RawMessage(`{"b":1,"a":2}`)); got == nil || *got != `{"a":2,"b":1}` {
		t.Errorf("object must be sorted/compact: %v", got)
	}
	long, _ := json.Marshal(string(make([]byte, 0)) + string(bytesRepeat('x', 305)))
	if got := fmtVal(long); got == nil || len([]rune(*got)) != 301 || (*got)[len(*got)-3:] != "…" {
		t.Errorf("truncation: len=%d", len([]rune(*got)))
	}
}

func bytesRepeat(b byte, n int) []byte {
	s := make([]byte, n)
	for i := range s {
		s[i] = b
	}
	return s
}

func TestIsSens(t *testing.T) {
	cases := []struct {
		name string
		sens string
		key  string
		want bool
	}{
		{"key true", `{"password": true}`, "password", true},
		{"key false", `{"password": false}`, "password", false},
		{"key missing", `{"other": true}`, "password", false},
		{"nested non-empty dict", `{"block": {"secret": true}}`, "block", true},
		{"nested empty dict", `{"block": {}}`, "block", false},
		{"nested non-empty list", `{"items": [true]}`, "items", true},
		{"nested empty list", `{"items": []}`, "items", false},
		{"whole value sensitive (bool true)", `true`, "anything", true},
		{"whole value not sensitive (bool false)", `false`, "anything", false},
		{"empty mask", ``, "anything", false},
	}
	for _, c := range cases {
		if got := isSens(json.RawMessage(c.sens), c.key); got != c.want {
			t.Errorf("%s: isSens(%q,%q) = %v, want %v", c.name, c.sens, c.key, got, c.want)
		}
	}
}

func TestModuleRefs(t *testing.T) {
	planJSON := `{
		"configuration": {
			"root_module": {
				"module_calls": {
					"vpc":   {"source": "terraform-aws-modules/vpc/aws", "version_constraint": "~> 5.0"},
					"acme":  {"source": "app.terraform.io/acme/network/aws", "version_constraint": "1.2.0"},
					"local": {"source": "./modules/db"},
					"vcs":   {"source": "git::https://example.com/mod.git"},
					"gh":    {"source": "github.com/org/repo"}
				}
			}
		}
	}`
	var plan Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// module_calls is a map (unordered); index by module_source to assert.
	got := map[string]ModuleRef{}
	for _, r := range ModuleRefs(&plan, nil) {
		got[r.ModuleSource] = r
	}
	if len(got) != 2 {
		t.Fatalf("captured %d registry modules, want 2 (local/git/github skipped): %+v", len(got), got)
	}
	if r := got["terraform-aws-modules/vpc/aws"]; r.RegistryHost != "registry.terraform.io" {
		t.Errorf("bare source must resolve to the public registry, got host %q", r.RegistryHost)
	}
	if r := got["acme/network/aws"]; r.RegistryHost != "app.terraform.io" {
		t.Errorf("host-prefixed source must keep its host + strip it from module_source, got %+v", r)
	}
	for src, r := range got {
		if r.ModuleVersion != nil {
			t.Errorf("module_version must be nil (constraint-only, no lockfile): %s = %v", src, *r.ModuleVersion)
		}
	}
}

func TestModuleRefs_NilAndEmpty(t *testing.T) {
	if refs := ModuleRefs(nil, nil); len(refs) != 0 {
		t.Errorf("nil plan → no refs, got %v", refs)
	}
	if refs := ModuleRefs(&Plan{}, nil); len(refs) != 0 {
		t.Errorf("plan with no configuration → no refs, got %v", refs)
	}
}

// TestModuleRefs_CanonicalizesHost proves capture folds host variants (via the
// shared suite.CanonicalHost) so the stored registry_host matches the registry's
// emitted (also-canonical) join key.
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
	for _, r := range ModuleRefs(&plan, nil) {
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

// TestModuleRefs_MalformedHostSkipped proves a host-prefixed source whose host
// segment carries userinfo (rejected by suite.CanonicalHost, which returns ""
// for it — see terraform-suite-identity#63) is skipped entirely rather than
// captured with an empty RegistryHost. Regression test for issue #175: before
// the fix, registryModuleAddress returned ok=true unconditionally in the
// 4-segment case, so a malformed host produced RegistryHost="" instead of
// being rejected like any other unparseable host.
func TestModuleRefs_MalformedHostSkipped(t *testing.T) {
	planJSON := `{
		"configuration": {
			"root_module": {
				"module_calls": {
					"vpc": {"source": "user:pass@evil.com:1234/myorg/vpc/aws", "version_constraint": "1.0.0"}
				}
			}
		}
	}`
	var plan Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, r := range ModuleRefs(&plan, nil) {
		if r.ModuleSource == "myorg/vpc/aws" {
			t.Fatalf("expected the malformed-host source to be skipped, got captured ref: %+v", r)
		}
	}
}

// modulesManifest is a representative .terraform/modules/modules.json: a root
// entry (skipped — no version), two registry modules (one public, one
// host-prefixed with mixed case to prove canonicalization), and a local module
// (skipped — no version / non-registry source).
const modulesManifest = `{
	"Modules": [
		{"Key": "", "Source": "", "Dir": "."},
		{"Key": "vpc", "Source": "terraform-aws-modules/vpc/aws", "Version": "5.3.0", "Dir": ".terraform/modules/vpc"},
		{"Key": "net", "Source": "App.Terraform.io/acme/network/aws", "Version": "1.2.0", "Dir": ".terraform/modules/net"},
		{"Key": "db", "Source": "./modules/db", "Dir": "modules/db"}
	]
}`

func TestParseModuleLocks(t *testing.T) {
	locks := ParseModuleLocks([]byte(modulesManifest))
	if locks == nil {
		t.Fatal("expected locks, got nil")
	}
	if got := locks[lockKey("registry.terraform.io", "terraform-aws-modules/vpc/aws")]; got != "5.3.0" {
		t.Errorf("public module version = %q, want 5.3.0", got)
	}
	// Host folded to lowercase via the shared canonicalizer; source host-stripped.
	if got := locks[lockKey("app.terraform.io", "acme/network/aws")]; got != "1.2.0" {
		t.Errorf("host-prefixed module version = %q, want 1.2.0", got)
	}
	if len(locks) != 2 {
		t.Errorf("want 2 locked registry modules (root/local skipped), got %d: %v", len(locks), locks)
	}
}

func TestParseModuleLocks_EmptyAndInvalid(t *testing.T) {
	if ParseModuleLocks(nil) != nil {
		t.Error("nil input → nil locks")
	}
	if ParseModuleLocks([]byte(`{not json`)) != nil {
		t.Error("invalid JSON → nil locks (degrade to constraint-only)")
	}
	// A manifest of only local/versionless modules yields nil (nothing to lock).
	if ParseModuleLocks([]byte(`{"Modules":[{"Key":"db","Source":"./db","Dir":"db"}]}`)) != nil {
		t.Error("no registry versions → nil locks")
	}
}

// TestModuleRefs_WithLocks proves an ingested module manifest fills resolved
// versions onto the matching refs, joined on the canonical (host, source) — and
// that a ref with no manifest entry stays version-nil (honest constraint-only).
func TestModuleRefs_WithLocks(t *testing.T) {
	planJSON := `{
		"configuration": {
			"root_module": {
				"module_calls": {
					"vpc":  {"source": "terraform-aws-modules/vpc/aws", "version_constraint": "~> 5.0"},
					"net":  {"source": "app.terraform.io/acme/network/aws", "version_constraint": "~> 1.0"},
					"only": {"source": "hashicorp/consul/aws", "version_constraint": "~> 0.1"}
				}
			}
		}
	}`
	var plan Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := map[string]ModuleRef{}
	for _, r := range ModuleRefs(&plan, ParseModuleLocks([]byte(modulesManifest))) {
		got[r.ModuleSource] = r
	}
	if r := got["terraform-aws-modules/vpc/aws"]; r.ModuleVersion == nil || *r.ModuleVersion != "5.3.0" {
		t.Errorf("vpc resolved version not filled: %+v", r)
	}
	if r := got["acme/network/aws"]; r.ModuleVersion == nil || *r.ModuleVersion != "1.2.0" {
		t.Errorf("host-prefixed resolved version not filled (canonical-host join): %+v", r)
	}
	if r := got["hashicorp/consul/aws"]; r.ModuleVersion != nil {
		t.Errorf("module absent from the manifest must stay version-nil, got %v", *r.ModuleVersion)
	}
}

// TestChangedAttrs_SensitivityUnion is the guard for the redaction gap fixed
// alongside @4cloudguru/terraform-drift-contract v1.1.0: an attribute marked
// sensitive on EITHER before_sensitive or after_sensitive is masked on BOTH
// sides. Masking each side against its own mirror emitted the unmarked side in
// cleartext, and a one-sided mark is the ROUTINE shape — terraform applies a
// config-derived mark (a `sensitive = true` variable, sensitive(), a sensitive
// module output) to the planned value only and never persists it to state.
//
// Rows were verified by inverting the guard (restoring the per-side masking):
// every union row below fails, and no fail-open row does.
func TestChangedAttrs_SensitivityUnion(t *testing.T) {
	const (
		oldSecret = "old-plaintext-SECRET"
		newSecret = "new-plaintext-SECRET"
		masked    = "(sensitive)"
	)
	cases := []struct {
		name       string
		beforeSens string
		afterSens  string
		wantBefore string
		wantAfter  string
	}{
		// --- the fix: a one-sided mark masks both sides -----------------------
		{"marked on before only", `{"k":true}`, `{"k":false}`, masked, masked},
		{"marked on after only", `{"k":false}`, `{"k":true}`, masked, masked},
		{"marked on both (unchanged behaviour)", `{"k":true}`, `{"k":true}`, masked, masked},
		{"before mirror absent, after marks", ``, `{"k":true}`, masked, masked},
		{"after mirror absent, before marks", `{"k":true}`, ``, masked, masked},
		{"nested non-empty dict on before only", `{"k":{"n":true}}`, `{"k":{}}`, masked, masked},
		{"nested non-empty list on after only", `{"k":[]}`, `{"k":[true]}`, masked, masked},
		{"whole-value mark on before only", `true`, `false`, masked, masked},
		{"whole-value mark on after only", `false`, `true`, masked, masked},

		// --- deliberately fail-open: these must NOT start masking -------------
		// Neither mirror present: emitted as-is. Masking here would mask every
		// attribute of every plan that carries no sensitivity metadata, and would
		// diverge from the contract (see its SECURITY.md).
		{"neither mirror present", ``, ``, oldSecret, newSecret},
		{"mirrors present but key unmarked", `{"other":true}`, `{"other":true}`, oldSecret, newSecret},
		{"both mirrors explicitly false", `{"k":false}`, `{"k":false}`, oldSecret, newSecret},
		{"empty nested dict on both (falsey)", `{"k":{}}`, `{"k":{}}`, oldSecret, newSecret},
		{"whole-value mark false on both", `false`, `false`, oldSecret, newSecret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := Change{
				Actions: []string{"update"},
				Before:  json.RawMessage(`{"k":"` + oldSecret + `"}`),
				After:   json.RawMessage(`{"k":"` + newSecret + `"}`),
			}
			if c.beforeSens != "" {
				ch.BeforeSensitive = json.RawMessage(c.beforeSens)
			}
			if c.afterSens != "" {
				ch.AfterSensitive = json.RawMessage(c.afterSens)
			}
			attrs := changedAttrs(ch)
			if len(attrs) != 1 || attrs[0].Name != "k" {
				t.Fatalf("attrs = %+v, want exactly one attr named k", attrs)
			}
			if attrs[0].Before == nil || *attrs[0].Before != c.wantBefore {
				t.Errorf("before = %v, want %q", deref(attrs[0].Before), c.wantBefore)
			}
			if attrs[0].After == nil || *attrs[0].After != c.wantAfter {
				t.Errorf("after = %v, want %q", deref(attrs[0].After), c.wantAfter)
			}
		})
	}
}

// TestChangedAttrs_MaskingHappensBeforeFmt proves a masked value is replaced
// wholesale rather than formatted: an oversized secret marked on one side only
// emits the bare literal, with no truncation marker and no fragment of the
// secret anywhere in the serialized summary.
func TestChangedAttrs_MaskingHappensBeforeFmt(t *testing.T) {
	secret := "S3CRET-" + strings.Repeat("x", 400) // > the 300-code-point cap
	plan := &Plan{ResourceChanges: []ResourceChange{{
		Address: "aws_instance.a",
		Change: Change{
			Actions:         []string{"update"},
			Before:          json.RawMessage(`{"user_data":"` + secret + `"}`),
			After:           json.RawMessage(`{"user_data":"` + secret + `Z"}`),
			BeforeSensitive: json.RawMessage(`{"user_data":true}`), // marked on ONE side
			AfterSensitive:  json.RawMessage(`{}`),
		},
	}}}
	out, err := json.Marshal(Summarize(plan).Summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "S3CRET") {
		t.Fatalf("secret reached the summary: %s", out)
	}
	if strings.Contains(string(out), "…") {
		t.Errorf("masked value must not be truncated (masking precedes fmt): %s", out)
	}
	if want := `"before":"(sensitive)","after":"(sensitive)"`; !strings.Contains(string(out), want) {
		t.Errorf("both sides must be masked, got %s", out)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
