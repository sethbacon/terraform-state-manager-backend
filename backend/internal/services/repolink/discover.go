// Package repolink provides the source-to-ADO repo-link auto-discovery seam.
//
// Linking a state source to its Azure DevOps repo/pipeline can be done two ways
// (decision O7): an operator sets it manually (the CRUD path, implemented today),
// or it is auto-discovered by querying Azure DevOps for repos/pipelines whose
// names match the source by convention. The live auto-discover path is
// credential-gated (it needs a configured, authenticated ADO client) and is
// deferred; this package provides the seam so it can be wired later without
// touching the API.
//
// The Discoverer interface is the seam. The default StubDiscoverer is
// unconfigured: it returns ErrNotConfigured so the discover endpoint can respond
// 503 ("discovery requires ADO configuration"), mirroring the inbound drift
// ingest endpoint's unconfigured behaviour. The ADODiscoverer is the best-effort
// implementation: given a configured ADO client it lists repositories and
// pipelines and ranks candidates by name-convention similarity to the source.
//
// Follow-up (live path): construct an authenticated ado.Client (PAT or, later,
// the WIF bearer used by the outbound drift-trigger) and pass it to
// NewADODiscoverer at wiring time. No live ADO auth is performed here.
package repolink

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
)

// ErrNotConfigured is returned by an unconfigured Discoverer (no ADO client).
// Callers map it to a 503 "discovery requires ADO configuration" response.
var ErrNotConfigured = errors.New("repolink: ADO auto-discovery is not configured")

// Candidate is a suggested source-to-repo link produced by auto-discovery. Score
// is a 0..1 name-convention match confidence; higher is a closer match. The
// pipeline fields are zero when no pipeline candidate was matched for the repo.
type Candidate struct {
	ADORepo       string  `json:"ado_repo"`
	ADOPipelineID *int    `json:"ado_pipeline_id,omitempty"`
	Score         float64 `json:"score"`
}

// Discoverer suggests ADO repo/pipeline candidates for a state source. It is the
// auto-discovery seam: the default StubDiscoverer is unconfigured and returns
// ErrNotConfigured; ADODiscoverer is the live (credential-gated) implementation.
type Discoverer interface {
	// Configured reports whether live discovery is available (an ADO client is
	// wired). When false, Discover returns ErrNotConfigured.
	Configured() bool
	// Discover returns candidate repo/pipeline links for the source name,
	// ordered best-match first. It returns ErrNotConfigured when unconfigured.
	Discover(ctx context.Context, sourceName string) ([]Candidate, error)
}

// StubDiscoverer is the default, unconfigured Discoverer. It performs no ADO
// calls and always reports unconfigured so the auto-discover endpoint returns
// 503 until a live ADO client is wired.
type StubDiscoverer struct{}

// NewStubDiscoverer returns the no-op default Discoverer.
func NewStubDiscoverer() *StubDiscoverer { return &StubDiscoverer{} }

// Configured always reports false for the stub.
func (StubDiscoverer) Configured() bool { return false }

// Discover always returns ErrNotConfigured for the stub.
func (StubDiscoverer) Discover(context.Context, string) ([]Candidate, error) {
	return nil, ErrNotConfigured
}

// ADOLister is the slice of the ADO client this package needs. It is satisfied
// by *ado.Client and is an interface so discovery is unit-testable with a fake.
type ADOLister interface {
	ListRepositories(ctx context.Context) ([]ado.Repository, error)
	ListPipelines(ctx context.Context) ([]ado.Pipeline, error)
}

// ADODiscoverer is the best-effort live Discoverer. Given a configured ADO
// client it lists the project's repositories and pipelines and ranks them by
// name-convention similarity to the source name. It never writes a link; it only
// returns candidates for an operator to confirm via the manual set endpoint.
type ADODiscoverer struct {
	client ADOLister
}

// NewADODiscoverer wraps a configured ADO client. Pass a nil client to obtain an
// unconfigured discoverer (Configured() reports false, Discover returns
// ErrNotConfigured) — this lets wiring code signal "no ADO credentials" without
// a separate stub.
func NewADODiscoverer(client ADOLister) *ADODiscoverer {
	return &ADODiscoverer{client: client}
}

// Configured reports whether an ADO client is wired.
func (d *ADODiscoverer) Configured() bool { return d.client != nil }

// minScore is the name-similarity floor below which a repository is not
// suggested. It avoids returning unrelated repos for a source with no match.
const minScore = 0.34

// Discover lists repositories and pipelines from the configured ADO project and
// returns candidates ranked by name-convention match to sourceName, best first.
// Only repositories scoring at or above minScore are returned. For each matched
// repository the closest-named pipeline (if any clears the threshold) is attached.
func (d *ADODiscoverer) Discover(ctx context.Context, sourceName string) ([]Candidate, error) {
	if !d.Configured() {
		return nil, ErrNotConfigured
	}

	repos, err := d.client.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	pipelines, err := d.client.ListPipelines(ctx)
	if err != nil {
		return nil, err
	}

	var candidates []Candidate
	for _, repo := range repos {
		score := nameScore(sourceName, repo.Name)
		if score < minScore {
			continue
		}
		c := Candidate{ADORepo: repo.Name, Score: score}
		if id := bestPipeline(repo.Name, pipelines); id != nil {
			c.ADOPipelineID = id
		}
		candidates = append(candidates, c)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	return candidates, nil
}

// bestPipeline returns the id of the pipeline whose name best matches repoName,
// or nil when none clears minScore.
func bestPipeline(repoName string, pipelines []ado.Pipeline) *int {
	bestScore := minScore
	var bestID *int
	for i := range pipelines {
		s := nameScore(repoName, pipelines[i].Name)
		if s >= bestScore {
			bestScore = s
			id := pipelines[i].ID
			bestID = &id
		}
	}
	return bestID
}

// nameScore returns a 0..1 convention-match score between two names. It is the
// Jaccard similarity of their tokenized, normalized word sets, which is robust
// to common separators (-, _, space) and case. An exact normalized match scores
// 1; entirely disjoint names score 0.
func nameScore(a, b string) float64 {
	sa := tokenSet(a)
	sb := tokenSet(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	intersection := 0
	for tok := range sa {
		if _, ok := sb[tok]; ok {
			intersection++
		}
	}
	union := len(sa) + len(sb) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// tokenSet lowercases name, splits it on common separators, and returns the set
// of non-empty tokens. Separators are '-', '_', '.', '/', and whitespace.
func tokenSet(name string) map[string]struct{} {
	fields := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		switch r {
		case '-', '_', '.', '/', ' ', '\t':
			return true
		default:
			return false
		}
	})
	set := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if f != "" {
			set[f] = struct{}{}
		}
	}
	return set
}
