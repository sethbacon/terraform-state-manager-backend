package driftingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// The Go half of the drift contract's shared conformance corpus.
//
// testdata/conformance/vectors.json is a byte-identical copy of
// conformance/vectors.json in 4cloudguru/terraform-drift-contract: one input
// plan per vector plus the EXPECTED output, hand-authored as intent on the
// canonical side. Before this file existed, "reconciled with the contract" meant
// a maintainer had read both implementations — which is how five semantic
// differences (key order, U+2028, HTML escaping, negative zero, and the jq row
// set) survived a review that said the mirrors were in lockstep.
//
// Neither CI job can run the other language, so agreement is anchored on
// literals that appear identically in both repositories:
//
//   - corpusSHA256      — the digest of the corpus file itself. Editing one copy
//     without the other reddens that repository.
//   - reconciledDigest  — a digest over the RENDERED results of every vector
//     with no stated per-implementation difference, produced through the same
//     documented discipline on both sides (fixed field order, attrs omitted
//     rather than null, `<`/`>`/`&` raw, U+2028/U+2029 escaped). One differing
//     byte anywhere in the reconciled set changes it on exactly one side.
//
// A vector carrying a `go` key records a difference that is KNOWN and written
// down — `rejects` when this implementation refuses the document at its
// unmarshal boundary, or `expect` when it answers differently. Those are
// excluded from the digest and asserted against their own stated expectation. A
// difference with no entry in the corpus is a regression.
//
// To change the corpus, see conformance/README.md in the contract repository:
// the semantic change, the vector and the three literals move together, in both
// repositories, in the same batch.
const (
	corpusPath   = "testdata/conformance/vectors.json"
	corpusSHA256 = "668a292a169dedfad131e98d44f7768159635112a7fbf2cf11a201ffb02e8daa"
	// Same literal as RECONCILED_DIGEST in the contract's __tests__/conformance.test.ts.
	reconciledDigest = "4f0002731219d9491636de981cde760688f720971d9a3882a2d6f55e13b6a173"
)

type conformStated struct {
	Rejects string          `json:"rejects"`
	Why     string          `json:"why"`
	Expect  json.RawMessage `json:"expect"`
}

type conformVector struct {
	ID     string          `json:"id"`
	Why    string          `json:"why"`
	Plan   json.RawMessage `json:"plan"`
	Expect json.RawMessage `json:"expect"`
	Go     *conformStated  `json:"go"`
}

// conformResult is the comparison envelope. The field order is the contract's
// and is what makes the rendered bytes comparable with the TypeScript side.
// A vector's `expect` states only the NON-DEFAULT markers, so the zero value of
// this struct IS the default — which is why every marker is named so that false
// and 0 are the ordinary answer.
type conformResult struct {
	Added          int            `json:"added"`
	Changed        int            `json:"changed"`
	Destroyed      int            `json:"destroyed"`
	Drifted        bool           `json:"drifted"`
	Unparseable    bool           `json:"unparseable"`
	Unmasked       bool           `json:"unmasked"`
	Truncated      bool           `json:"truncated"`
	OmittedEntries int            `json:"omitted_entries"`
	OmittedAttrs   int            `json:"omitted_attrs"`
	Summary        []SummaryEntry `json:"summary"`
}

// envelope projects a Result into the comparison shape, in one place, so the
// two call sites below cannot drift apart.
func envelope(r *Result) conformResult {
	return conformResult{
		Added: r.Added, Changed: r.Changed, Destroyed: r.Destroyed,
		Drifted: r.Drifted(), Unparseable: r.Unparseable, Unmasked: r.Unmasked,
		Truncated: r.Truncated(), OmittedEntries: r.OmittedEntries,
		OmittedAttrs: r.OmittedAttrs, Summary: r.Summary,
	}
}

// renderConform is the rendering discipline, shared by the actual and the
// expected side so a difference in either is a difference in the contract.
// SetEscapeHTML(false) matters: with the default encoder every `<` in an emitted
// value would be escaped by the HARNESS and the comparison would pass over a
// divergence in the implementation, or fail over one that is not there.
func renderConform(t *testing.T, r conformResult) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		t.Fatalf("rendering the comparison envelope failed: %v", err)
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func renderExpected(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var want conformResult
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("corpus expectation is not a Result: %v", err)
	}
	if want.Summary == nil {
		want.Summary = []SummaryEntry{}
	}
	return renderConform(t, want)
}

func loadCorpus(t *testing.T) []conformVector {
	t.Helper()
	raw, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("reading the conformance corpus: %v", err)
	}
	if got := hex.EncodeToString(sha256Sum(raw)); got != corpusSHA256 {
		t.Fatalf("the vendored corpus is not the contract's copy:\n got %s\nwant %s\n"+
			"re-vendor conformance/vectors.json from 4cloudguru/terraform-drift-contract "+
			"and update corpusSHA256 and reconciledDigest together", got, corpusSHA256)
	}
	var doc struct {
		Limits struct {
			MaxEntries       int `json:"max_entries"`
			MaxAttrsPerEntry int `json:"max_attrs_per_entry"`
		} `json:"limits"`
		Vectors []conformVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the conformance corpus: %v", err)
	}
	// The bounds are the contract's, not this package's. Changing one here
	// without the corpus (or the other way round) means the producers and the
	// ingester stop describing the same bound, and a consumer cannot tell a
	// capped summary from a complete one across them.
	if doc.Limits.MaxEntries != MaxEntries || doc.Limits.MaxAttrsPerEntry != MaxAttrsPerEntry {
		t.Fatalf("this package bounds the summary at %d/%d, the contract declares %d/%d",
			MaxEntries, MaxAttrsPerEntry, doc.Limits.MaxEntries, doc.Limits.MaxAttrsPerEntry)
	}
	// An empty corpus would make every assertion below vacuous.
	if len(doc.Vectors) < 40 {
		t.Fatalf("corpus has %d vectors, expected the full set", len(doc.Vectors))
	}
	return doc.Vectors
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// TestConformance_Corpus runs every vector through Summarize and compares the
// rendered result with the corpus expectation, byte for byte.
func TestConformance_Corpus(t *testing.T) {
	for _, vec := range loadCorpus(t) {
		t.Run(vec.ID, func(t *testing.T) {
			var plan Plan
			err := json.Unmarshal(vec.Plan, &plan)

			if vec.Go != nil && vec.Go.Rejects != "" {
				// The stated difference IS that this implementation refuses the
				// document. Assert the refusal, so a later change that starts
				// accepting it (and therefore answers something the corpus does
				// not describe) reddens here.
				if err == nil {
					t.Fatalf("the corpus states this document is rejected (%s) but json.Unmarshal accepted it", vec.Go.Rejects)
				}
				return
			}
			if err != nil {
				t.Fatalf("the corpus states no difference for this vector, but json.Unmarshal rejected it: %v", err)
			}

			got := renderConform(t, envelope(Summarize(&plan)))

			want := vec.Expect
			if vec.Go != nil && len(vec.Go.Expect) > 0 {
				want = vec.Go.Expect
			}
			if got != renderExpected(t, want) {
				t.Errorf("%s\n why: %s\n  got: %s\n want: %s", vec.ID, vec.Why, got, renderExpected(t, want))
			}
		})
	}
}

// TestConformance_ReconciledDigest is the cross-implementation anchor: the
// canonical TypeScript runner computes this same digest over the same vectors in
// the same order and asserts the same literal, so the two agree byte-for-byte
// without either CI job running the other language.
func TestConformance_ReconciledDigest(t *testing.T) {
	h := sha256.New()
	n := 0
	for _, vec := range loadCorpus(t) {
		if vec.Go != nil {
			continue // a stated difference; asserted individually above
		}
		var plan Plan
		if err := json.Unmarshal(vec.Plan, &plan); err != nil {
			t.Fatalf("%s: %v", vec.ID, err)
		}
		rendered := renderConform(t, envelope(Summarize(&plan)))
		h.Write([]byte(vec.ID + "\n" + rendered + "\n"))
		n++
	}
	if n < 40 {
		t.Fatalf("only %d vectors in the reconciled subset; the digest would not establish much", n)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != reconciledDigest {
		t.Errorf("the reconciled subset does not match the canonical contract:\n got %s\nwant %s\n"+
			"one of the %d vectors renders differently here than in "+
			"@4cloudguru/terraform-drift-contract; TestConformance_Corpus names which",
			got, reconciledDigest, n)
	}
}
