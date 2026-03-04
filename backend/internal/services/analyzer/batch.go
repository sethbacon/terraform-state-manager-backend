package analyzer

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// BatchConfig controls batch processing behavior.
type BatchConfig struct {
	MaxWorkers int
	BatchSize  int
}

// DefaultBatchConfig returns the default batch processing configuration.
func DefaultBatchConfig() BatchConfig {
	return BatchConfig{MaxWorkers: 5, BatchSize: 20}
}

// WorkspaceResult holds the analysis result for a single workspace.
type WorkspaceResult struct {
	Workspace hcp.Workspace   `json:"workspace"`
	Counts    *ResourceCounts `json:"counts,omitempty"`
	Error     *AnalysisError  `json:"error,omitempty"`
	Method    string          `json:"method"`
}

// ProcessWorkspaces processes a list of workspaces concurrently using a semaphore.
// It downloads each workspace's state, parses it, and counts resources.
// Concurrency is limited by cfg.MaxWorkers using a weighted semaphore.
func ProcessWorkspaces(ctx context.Context, client *hcp.Client, workspaces []hcp.Workspace, cfg BatchConfig) ([]WorkspaceResult, error) {
	if len(workspaces) == 0 {
		return []WorkspaceResult{}, nil
	}

	maxWorkers := cfg.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = DefaultBatchConfig().MaxWorkers
	}

	sem := semaphore.NewWeighted(int64(maxWorkers))
	g, gctx := errgroup.WithContext(ctx)

	var mu sync.Mutex
	results := make([]WorkspaceResult, 0, len(workspaces))

	for _, ws := range workspaces {
		g.Go(func() error {
			// Acquire semaphore slot.
			if err := sem.Acquire(gctx, 1); err != nil {
				// Context was canceled while waiting for a slot.
				mu.Lock()
				results = append(results, WorkspaceResult{
					Workspace: ws,
					Error:     ClassifyError(err),
					Method:    "hcp_state_download",
				})
				mu.Unlock()
				return nil
			}
			defer sem.Release(1)

			result := processWorkspace(gctx, client, ws)

			mu.Lock()
			results = append(results, result)
			mu.Unlock()

			return nil
		})
	}

	// Wait for all goroutines to complete.
	if err := g.Wait(); err != nil {
		return results, err
	}

	return results, nil
}

// processWorkspace handles the analysis of a single workspace.
// It downloads the state file using the workspace's StateDownloadURL,
// counts resources, and merges workspace metadata into the result.
func processWorkspace(ctx context.Context, client *hcp.Client, ws hcp.Workspace) WorkspaceResult {
	result := WorkspaceResult{
		Workspace: ws,
		Method:    "hcp_state_download",
	}

	// Check if the workspace has a state download URL.
	downloadURL := ws.StateDownloadURL
	if downloadURL == "" {
		// Try to fetch the current state version to get the download URL.
		if ws.ID == "" {
			result.Error = NewAnalysisError(ErrorTypeStateNotFound, "workspace has no state download URL and no ID", nil)
			return result
		}
		stateVersion, err := client.GetCurrentStateVersion(ctx, ws.ID)
		if err != nil {
			result.Error = ClassifyError(err)
			return result
		}
		downloadURL = stateVersion.DownloadURL
		if downloadURL == "" {
			result.Error = NewAnalysisError(ErrorTypeStateNotFound, "no state download URL available for workspace", nil)
			return result
		}
	}

	// Download the state file. DownloadState returns a parsed *hcp.StateFile.
	stateFile, err := client.DownloadState(ctx, downloadURL)
	if err != nil {
		result.Error = ClassifyError(err)
		return result
	}

	if stateFile == nil {
		result.Error = NewAnalysisError(ErrorTypeStateNotFound, "nil state file returned", nil)
		return result
	}

	// Count resources from the state file.
	var counts *ResourceCounts
	if len(stateFile.Resources) > 0 {
		counts = CountResources(stateFile.Resources)
	} else if len(stateFile.Modules) > 0 {
		counts = parseLegacyModules(stateFile.Modules)
	} else {
		counts = NewResourceCounts()
	}

	// Enrich with state file metadata.
	counts.TerraformVersion = stateFile.TerraformVersion
	counts.Serial = stateFile.Serial
	counts.Lineage = stateFile.Lineage
	counts.ProviderAnalysis = ExtractProviderAnalysis(stateFile)

	// Extract workspace metadata and merge it into the counts.
	wsMeta := ExtractWorkspaceMetadata(ws)
	counts = MergeMetadata(counts, wsMeta, stateFile)

	result.Counts = counts
	return result
}

// ProcessWorkspacesInBatches splits workspaces into batches and processes each batch.
// This is useful for very large workspace lists where you want to control memory usage.
func ProcessWorkspacesInBatches(ctx context.Context, client *hcp.Client, workspaces []hcp.Workspace, cfg BatchConfig) ([]WorkspaceResult, error) {
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchConfig().BatchSize
	}

	allResults := make([]WorkspaceResult, 0, len(workspaces))

	for i := 0; i < len(workspaces); i += batchSize {
		end := i + batchSize
		if end > len(workspaces) {
			end = len(workspaces)
		}
		batch := workspaces[i:end]

		batchResults, err := ProcessWorkspaces(ctx, client, batch, cfg)
		if err != nil {
			return allResults, err
		}
		allResults = append(allResults, batchResults...)

		// Check for context cancellation between batches.
		select {
		case <-ctx.Done():
			return allResults, ctx.Err()
		default:
		}
	}

	return allResults, nil
}
