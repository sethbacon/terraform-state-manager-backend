package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The jq half of the drift contract's shared conformance corpus.
//
// The dispatched CI templates are the third implementation of the contract, and
// the only one that is a shell script rather than a program: they compute the
// counts, `drifted` and the summary with inline jq and POST them to the same
// callback the report action and /drift/ingest feed. Nothing ran them over the
// contract's vectors, which is how two divergences survived — the summary kept
// resources whose actions were exactly ["read"], and `drifted` came from
// `terraform plan -detailed-exitcode` (2 for a non-empty diff of ANY kind,
// including an output-only change with no resource_changes entries at all).
//
// The corpus is read from the SAME file the Go mirror runs, deliberately: one
// copy in this repository, so the two cannot answer different questions.
//
// The jq shape is narrower than the contract's by design — a dispatched row is
// {address, actions} and carries no attribute values at all, so it has nothing
// to mask. The expectation for a vector is therefore `expect` with every attrs
// array dropped, unless the vector STATES a jq difference.
const jqCorpusPath = "../services/driftingest/testdata/conformance/vectors.json"

// Same literal as PROVENANCE_DIGEST in the contract's
// __tests__/conformance.test.ts. moduleCallsPlan() and this jq are the two
// implementations of the provenance projection, so this is where they are
// compared byte-for-byte.
const provenanceDigest = "102777523913f3d90fb5a1a0bd7860e9b96c8b42f31ac30ceef13ad6ab1bcc3c"

// The shipped summarizer, matched verbatim in the template text so these tests
// exercise the exact lines a runner executes.
var summarizeBlockRe = regexp.MustCompile(`(?s)(ADD=\$\(jq .*?UNMASKED=\$\(jq -c '.*?' plan\.json\))`)

func summarizeBlock(t *testing.T, tmpl string) string {
	t.Helper()
	m := summarizeBlockRe.FindStringSubmatch(tmpl)
	if m == nil {
		t.Fatal("template has no ADD=…/UNMASKED=$(jq …) summarizer block")
	}
	return m[1]
}

type jqStated struct {
	Why    string          `json:"why"`
	Expect json.RawMessage `json:"expect"`
}

type jqVector struct {
	ID                string          `json:"id"`
	Why               string          `json:"why"`
	Plan              json.RawMessage `json:"plan"`
	Expect            json.RawMessage `json:"expect"`
	ExpectModuleCalls json.RawMessage `json:"expect_module_calls"`
	JQ                *jqStated       `json:"jq"`
}

// jqEnvelope is the payload shape the templates compose with `jq -n`, in the
// order they compose it, so the rendered expectation is comparable with the jq
// output byte for byte rather than merely value for value.
type jqEnvelope struct {
	Added          int     `json:"added"`
	Changed        int     `json:"changed"`
	Destroyed      int     `json:"destroyed"`
	Drifted        bool    `json:"drifted"`
	Unparseable    bool    `json:"unparseable"`
	Unmasked       bool    `json:"unmasked"`
	Truncated      bool    `json:"truncated"`
	OmittedEntries int     `json:"omitted_entries"`
	OmittedAttrs   int     `json:"omitted_attrs"`
	Summary        []jqRow `json:"summary"`
}

// Address and Actions are `any` because two vectors deliberately carry a
// non-string address and a non-array actions: the dispatched jq copies both
// through with no type check, and that is a STATED difference, not something to
// normalise away in the harness.
type jqRow struct {
	Address any `json:"address"`
	Actions any `json:"actions"`
}

func marshalNoHTMLEscape(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("rendering the comparison envelope failed: %v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func loadJQCorpus(t *testing.T) []jqVector {
	t.Helper()
	raw, err := os.ReadFile(jqCorpusPath)
	if err != nil {
		t.Fatalf("reading the conformance corpus: %v", err)
	}
	var doc struct {
		Limits struct {
			MaxEntries int `json:"max_entries"`
		} `json:"limits"`
		Vectors []jqVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the conformance corpus: %v", err)
	}
	// The dispatched templates hard-code the row cap in the jq itself, so the
	// only way it stays the contract's number is to assert the two agree.
	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		want := fmt.Sprintf(".[0:%d]", doc.Limits.MaxEntries)
		if !strings.Contains(tmpl, want) {
			t.Errorf("%s template does not cap the summary at the contract's %d rows (%q)", name, doc.Limits.MaxEntries, want)
		}
	}
	// The driftingest side pins the file's SHA-256; an empty read here would
	// still make every assertion below vacuous.
	if len(doc.Vectors) < 40 {
		t.Fatalf("corpus has %d vectors, expected the full set", len(doc.Vectors))
	}
	return doc.Vectors
}

// expectedJQ renders what the dispatched summarizer must emit for a vector:
// either the stated jq expectation, or the contract's Result with every attrs
// array dropped.
func expectedJQ(t *testing.T, vec jqVector) string {
	t.Helper()
	raw := vec.Expect
	if vec.JQ != nil && len(vec.JQ.Expect) > 0 {
		raw = vec.JQ.Expect
	}
	var want jqEnvelope
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("%s: corpus expectation is not a Result: %v", vec.ID, err)
	}
	if want.Summary == nil {
		want.Summary = []jqRow{}
	}
	return marshalNoHTMLEscape(t, want)
}

// runSummarizer runs the extracted summarizer over one plan and returns the
// callback payload's count/summary half, composed exactly as the template
// composes it (`jq -n --argjson …`, never string concatenation).
func runSummarizer(t *testing.T, jq, block string, plan []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), plan, 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	const emit = "\njq -n -c --argjson added \"$ADD\" --argjson changed \"$CHG\" " +
		"--argjson destroyed \"$DEL\" --argjson drifted \"$DRIFTED\" --argjson summary \"$SUMMARY\" " +
		"--argjson unparseable \"$UNPARSEABLE\" --argjson unmasked \"$UNMASKED\" " +
		"--argjson truncated \"$TRUNCATED\" --argjson omitted_entries \"$OMITTED_ENTRIES\" " +
		"'{added:$added, changed:$changed, destroyed:$destroyed, drifted:$drifted, " +
		"unparseable:$unparseable, unmasked:$unmasked, truncated:$truncated, " +
		"omitted_entries:$omitted_entries, omitted_attrs:0, summary:$summary}'"
	cmd := exec.Command("bash", "-c", block+emit)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(jq)+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the shipped summarizer failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestConformance_DispatchedSummarizer runs both shipped templates' summarizers
// over every corpus vector and compares the composed payload with the contract's
// expectation.
func TestConformance_DispatchedSummarizer(t *testing.T) {
	jq := requireJQ(t)
	vectors := loadJQCorpus(t)

	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		block := summarizeBlock(t, tmpl)
		t.Run(name, func(t *testing.T) {
			for _, vec := range vectors {
				t.Run(vec.ID, func(t *testing.T) {
					got := runSummarizer(t, jq, block, vec.Plan)
					if want := expectedJQ(t, vec); got != want {
						why := vec.Why
						if vec.JQ != nil {
							why = vec.JQ.Why
						}
						t.Errorf("%s\n why: %s\n  got: %s\n want: %s", vec.ID, why, got, want)
					}
				})
			}
		})
	}
}

// TestConformance_DispatchedProvenance is the byte-for-byte anchor between
// moduleCallsPlan() in the contract and the MODULE_CALLS jq here: the same
// digest literal is asserted on both sides, so a projection, a scrub, a cap or a
// key ORDER that differs reddens one repository or the other.
func TestConformance_DispatchedProvenance(t *testing.T) {
	jq := requireJQ(t)
	vectors := loadJQCorpus(t)

	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		block := moduleCallsBlock(t, tmpl)
		t.Run(name, func(t *testing.T) {
			h := sha256.New()
			n := 0
			for _, vec := range vectors {
				if len(vec.ExpectModuleCalls) == 0 {
					continue
				}
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "plan.json"), vec.Plan, 0o600); err != nil {
					t.Fatalf("write plan: %v", err)
				}
				got := runShippedBlock(t, dir, jq, block, "MODULE_CALLS")
				var want any
				if err := json.Unmarshal(vec.ExpectModuleCalls, &want); err != nil {
					t.Fatalf("%s: %v", vec.ID, err)
				}
				h.Write([]byte(vec.ID + "\n" + got + "\n"))
				n++
			}
			if n < 3 {
				t.Fatalf("only %d provenance vectors; the digest would not establish much", n)
			}
			if digest := hex.EncodeToString(h.Sum(nil)); digest != provenanceDigest {
				t.Errorf("the dispatched provenance does not match moduleCallsPlan():\n got %s\nwant %s",
					digest, provenanceDigest)
			}
		})
	}
}

// TestConformance_DispatchedSummaryIsBounded covers what no shared vector can:
// the corpus declares the row cap and every implementation asserts the NUMBER,
// but a 501-entry plan committed as a vector would be 150 KB of noise, so the
// tripping behaviour is exercised here.
func TestConformance_DispatchedSummaryIsBounded(t *testing.T) {
	jq := requireJQ(t)

	changes := make([]string, 0, 503)
	for i := 0; i < 503; i++ {
		changes = append(changes, fmt.Sprintf(`{"address":"aws_instance.a%d","change":{"actions":["create"]}}`, i))
	}
	plan := []byte(`{"resource_changes":[` + strings.Join(changes, ",") + `]}`)

	for name, tmpl := range map[string]string{"github": githubDriftWorkflow, "azure": azureDriftPipeline} {
		block := summarizeBlock(t, tmpl)
		t.Run(name, func(t *testing.T) {
			var got jqEnvelope
			if err := json.Unmarshal([]byte(runSummarizer(t, jq, block, plan)), &got); err != nil {
				t.Fatalf("payload is not valid JSON: %v", err)
			}
			if len(got.Summary) != 500 {
				t.Errorf("emitted %d rows, want the 500 cap", len(got.Summary))
			}
			if got.OmittedEntries != 3 || !got.Truncated {
				t.Errorf("omitted_entries=%d truncated=%v, want 3/true", got.OmittedEntries, got.Truncated)
			}
			// The counts are NOT capped: capping them would turn a payload bound
			// into a missed detection, and `drifted` is derived from them.
			if got.Added != 503 || !got.Drifted {
				t.Errorf("added=%d drifted=%v, want 503/true", got.Added, got.Drifted)
			}
		})
	}
}

// TestConformance_InfraDriftPayloadKeysMatch is the conformance property for
// migration 000039's four columns, on the two receiving DTOs rather than on the
// jq producers above: driftRunResultPayload (the dispatched-run callback) and
// driftIngestPayload (the push endpoint) are two independent Go types, and
// nothing but convention keeps their JSON vocabulary for the SAME four
// contract fields identical. A rename on one side would compile cleanly (they
// share no embedded struct for these fields the way Completeness is shared)
// and would only surface as two producers disagreeing about a wire shape --
// exactly the defect class Completeness's shared type was introduced to
// prevent (see drift_completeness.go). This pins the keys directly instead of
// trusting that the two literals were typed identically.
func TestConformance_InfraDriftPayloadKeysMatch(t *testing.T) {
	want := []string{"drift_added", "drift_changed", "drift_destroyed", "drift_summary"}

	runKeys := decodedJSONKeys(reflect.TypeOf(driftRunResultPayload{}))
	ingestKeys := decodedJSONKeys(reflect.TypeOf(driftIngestPayload{}))
	if len(runKeys) == 0 || len(ingestKeys) == 0 {
		t.Fatal("no json tags found on one of the payload DTOs; the guard would pass vacuously")
	}
	for _, k := range want {
		if !runKeys[k] {
			t.Errorf("driftRunResultPayload does not decode %q", k)
		}
		if !ingestKeys[k] {
			t.Errorf("driftIngestPayload does not decode %q", k)
		}
	}
}
