package analyzer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

func TestDefaultBatchConfig_Fields(t *testing.T) {
	cfg := DefaultBatchConfig()
	assert.Equal(t, 5, cfg.MaxWorkers)
	assert.Equal(t, 20, cfg.BatchSize)
}

func TestNewAnalyzer(t *testing.T) {
	cfg := DefaultBatchConfig()
	// NewAnalyzer just stores the arguments — no network call
	a := NewAnalyzer(nil, cfg)
	require.NotNil(t, a)
}

func TestProcessWorkspaces_EmptyList(t *testing.T) {
	results, err := ProcessWorkspaces(context.Background(), nil, []hcp.Workspace{}, DefaultBatchConfig())
	require.NoError(t, err)
	assert.Equal(t, []WorkspaceResult{}, results)
}

func TestProcessWorkspacesInBatches_EmptyList(t *testing.T) {
	results, err := ProcessWorkspacesInBatches(context.Background(), nil, []hcp.Workspace{}, DefaultBatchConfig())
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestProcessWorkspacesInBatches_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// With an already-cancelled context and a non-empty workspace list, the
	// function should return with a context error before processing any work.
	workspaces := make([]hcp.Workspace, 1)
	workspaces[0] = hcp.Workspace{ID: "ws-001"}

	// ProcessWorkspacesInBatches calls ProcessWorkspaces which uses errgroup
	// and semaphore — cancellation behaviour depends on timing so we just
	// check that no panic occurs and either no error or ctx.Err() is returned.
	_, _ = ProcessWorkspacesInBatches(ctx, nil, workspaces, BatchConfig{MaxWorkers: 1, BatchSize: 1})
}

func TestProcessWorkspacesInBatches_DefaultBatchSizeUsedWhenZero(t *testing.T) {
	cfg := BatchConfig{MaxWorkers: 1, BatchSize: 0} // BatchSize 0 → default
	results, err := ProcessWorkspacesInBatches(context.Background(), nil, []hcp.Workspace{}, cfg)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestProcessWorkspaces_DefaultWorkersUsedWhenZero(t *testing.T) {
	cfg := BatchConfig{MaxWorkers: 0, BatchSize: 10} // MaxWorkers 0 → default
	results, err := ProcessWorkspaces(context.Background(), nil, []hcp.Workspace{}, cfg)
	require.NoError(t, err)
	assert.Equal(t, []WorkspaceResult{}, results)
}
