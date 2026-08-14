package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
var summarizeBlockRe = regexp.MustCompile(`(?s)(ADD=\$\(jq .*?SUMMARY=\$\(jq -c '.*?' plan\.json\))`)

func summarizeBlock(t *testing.T, tmpl string) string {
	t.Helper()
	m := summarizeBlockRe.FindStringSubmatch(tmpl)
	if m == nil {
		t.Fatal("template has no ADD=…/SUMMARY=$(jq …) summarizer block")
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
	Added     int     `json:"added"`
	Changed   int     `json:"changed"`
	Destroyed int     `json:"destroyed"`
	Drifted   bool    `json:"drifted"`
	Summary   []jqRow `json:"summary"`
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
		Vectors []jqVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the conformance corpus: %v", err)
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
		"'{added:$added, changed:$changed, destroyed:$destroyed, drifted:$drifted, summary:$summary}'"
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
