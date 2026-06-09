package repolink

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
)

// fakeLister is a hand-rolled ADOLister for unit tests; it returns canned
// repositories/pipelines and optional errors without any live ADO call.
type fakeLister struct {
	repos     []ado.Repository
	pipelines []ado.Pipeline
	repoErr   error
	pipeErr   error
}

func (f *fakeLister) ListRepositories(context.Context) ([]ado.Repository, error) {
	return f.repos, f.repoErr
}
func (f *fakeLister) ListPipelines(context.Context) ([]ado.Pipeline, error) {
	return f.pipelines, f.pipeErr
}

func TestStubDiscoverer_NotConfigured(t *testing.T) {
	d := NewStubDiscoverer()
	assert.False(t, d.Configured())
	_, err := d.Discover(context.Background(), "anything")
	require.ErrorIs(t, err, ErrNotConfigured)
}

func TestADODiscoverer_NilClient_NotConfigured(t *testing.T) {
	d := NewADODiscoverer(nil)
	assert.False(t, d.Configured())
	_, err := d.Discover(context.Background(), "anything")
	require.ErrorIs(t, err, ErrNotConfigured)
}

func TestADODiscoverer_RanksByNameConvention(t *testing.T) {
	lister := &fakeLister{
		repos: []ado.Repository{
			{Name: "tf-network"},
			{Name: "unrelated-billing"},
			{Name: "network-legacy"},
		},
		pipelines: []ado.Pipeline{
			{ID: 11, Name: "tf-network-plan"},
			{ID: 22, Name: "billing-deploy"},
		},
	}
	d := NewADODiscoverer(lister)
	require.True(t, d.Configured())

	candidates, err := d.Discover(context.Background(), "tf-network")
	require.NoError(t, err)
	require.NotEmpty(t, candidates)

	// Best match is the exact-token repo, ranked first, with its plan pipeline.
	assert.Equal(t, "tf-network", candidates[0].ADORepo)
	require.NotNil(t, candidates[0].ADOPipelineID)
	assert.Equal(t, 11, *candidates[0].ADOPipelineID)
	assert.Greater(t, candidates[0].Score, 0.0)

	// The fully-unrelated repo is filtered out (below the score floor).
	for _, c := range candidates {
		assert.NotEqual(t, "unrelated-billing", c.ADORepo)
	}
}

func TestADODiscoverer_NoMatch_ReturnsEmpty(t *testing.T) {
	lister := &fakeLister{
		repos:     []ado.Repository{{Name: "completely-different-thing"}},
		pipelines: nil,
	}
	d := NewADODiscoverer(lister)

	candidates, err := d.Discover(context.Background(), "tf-network")
	require.NoError(t, err)
	assert.Empty(t, candidates)
}

func TestADODiscoverer_ListReposError_Propagates(t *testing.T) {
	lister := &fakeLister{repoErr: errors.New("boom")}
	d := NewADODiscoverer(lister)

	_, err := d.Discover(context.Background(), "tf-network")
	require.Error(t, err)
}

func TestADODiscoverer_ListPipelinesError_Propagates(t *testing.T) {
	lister := &fakeLister{
		repos:   []ado.Repository{{Name: "tf-network"}},
		pipeErr: errors.New("boom"),
	}
	d := NewADODiscoverer(lister)

	_, err := d.Discover(context.Background(), "tf-network")
	require.Error(t, err)
}
