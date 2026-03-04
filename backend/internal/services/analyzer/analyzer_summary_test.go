package analyzer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

func TestNewEmptySummary(t *testing.T) {
	s := newEmptySummary()
	require.NotNil(t, s)
	assert.NotNil(t, s.Organizations)
	assert.NotNil(t, s.TopResourceTypes)
	assert.Equal(t, 0, s.TotalRUM)
	assert.Equal(t, 0, s.TotalResources)
}

func TestBuildSummary_Empty(t *testing.T) {
	s := buildSummary([]WorkspaceResult{})
	require.NotNil(t, s)
	assert.Equal(t, 0, s.TotalRUM)
	assert.Equal(t, 0, s.TotalWorkspaces)
}

func TestBuildSummary_SkipsErrorResults(t *testing.T) {
	results := []WorkspaceResult{
		{
			Workspace: hcp.Workspace{Organization: "myorg"},
			Error:     NewAnalysisError(ErrorTypeException, "failed", nil),
		},
	}

	s := buildSummary(results)
	require.NotNil(t, s)
	assert.Equal(t, 0, s.TotalWorkspaces)
	assert.Equal(t, 0, s.TotalRUM)
}

func TestBuildSummary_SkipsNilCounts(t *testing.T) {
	results := []WorkspaceResult{
		{
			Workspace: hcp.Workspace{Organization: "myorg"},
			Counts:    nil,
		},
	}

	s := buildSummary(results)
	assert.Equal(t, 0, s.TotalRUM)
}

func TestBuildSummary_AggregatesSuccessfulResults(t *testing.T) {
	counts1 := &ResourceCounts{
		Total:       3,
		Managed:     3,
		RUM:         2,
		DataSources: 0,
		ByType:      map[string]int{"aws_instance": 2, "aws_s3_bucket": 1},
		ByModule:    map[string]int{"root": 3},
	}
	counts2 := &ResourceCounts{
		Total:       5,
		Managed:     4,
		RUM:         4,
		DataSources: 1,
		ByType:      map[string]int{"aws_instance": 3, "google_compute_instance": 1},
		ByModule:    map[string]int{"root": 5},
	}

	results := []WorkspaceResult{
		{Workspace: hcp.Workspace{Organization: "orgA"}, Counts: counts1},
		{Workspace: hcp.Workspace{Organization: "orgB"}, Counts: counts2},
	}

	s := buildSummary(results)
	require.NotNil(t, s)

	assert.Equal(t, 2, s.TotalWorkspaces)
	assert.Equal(t, 6, s.TotalRUM)     // 2 + 4
	assert.Equal(t, 7, s.TotalManaged) // 3 + 4
	assert.Equal(t, 8, s.TotalResources)
	assert.Equal(t, 1, s.TotalDataSources)

	assert.Equal(t, 1, s.Organizations["orgA"])
	assert.Equal(t, 1, s.Organizations["orgB"])

	assert.Equal(t, 5, s.TopResourceTypes["aws_instance"]) // 2+3
	assert.Equal(t, 1, s.TopResourceTypes["aws_s3_bucket"])
}

func TestBuildSummary_ProviderSummaryAggregated(t *testing.T) {
	providerAnalysis := &ProviderAnalysis{
		ProviderVersions: map[string]string{"hashicorp/aws": "5.0.0"},
		ProviderUsage: map[string]*ProviderUsage{
			"hashicorp/aws": {
				ResourceCount: 3,
				ResourceTypes: []string{"aws_instance"},
				Modules:       []string{"root"},
			},
		},
		ProviderStatistics: &ProviderStatistics{TotalProviders: 1},
		VersionAnalysis:    &VersionAnalysis{TerraformVersion: "1.5.0"},
	}

	counts := &ResourceCounts{
		Total:            3,
		Managed:          3,
		RUM:              3,
		ByType:           map[string]int{"aws_instance": 3},
		ByModule:         map[string]int{"root": 3},
		ProviderAnalysis: providerAnalysis,
	}

	results := []WorkspaceResult{
		{Workspace: hcp.Workspace{Organization: "myorg"}, Counts: counts},
	}

	s := buildSummary(results)
	require.NotNil(t, s)
	require.NotNil(t, s.ProviderSummary)
	assert.Contains(t, s.ProviderSummary.AllProviders, "hashicorp/aws")
	assert.Equal(t, 1, s.ProviderSummary.AllProviders["hashicorp/aws"].WorkspacesUsing)
	assert.Equal(t, 1, s.ProviderSummary.TerraformVersions["1.5.0"])
}

func TestBuildSummary_NoProviderSummaryWhenNoProviders(t *testing.T) {
	counts := &ResourceCounts{
		Total:    1,
		Managed:  1,
		RUM:      1,
		ByType:   map[string]int{"aws_instance": 1},
		ByModule: map[string]int{"root": 1},
		// No ProviderAnalysis
	}

	results := []WorkspaceResult{
		{Workspace: hcp.Workspace{Organization: "myorg"}, Counts: counts},
	}

	s := buildSummary(results)
	assert.Nil(t, s.ProviderSummary, "ProviderSummary should be nil when no providers")
}

func TestBuildSummary_OrganizationCounter(t *testing.T) {
	counts := &ResourceCounts{
		ByType:   make(map[string]int),
		ByModule: make(map[string]int),
	}

	results := []WorkspaceResult{
		{Workspace: hcp.Workspace{Organization: "myorg"}, Counts: counts},
		{Workspace: hcp.Workspace{Organization: "myorg"}, Counts: counts},
		{Workspace: hcp.Workspace{Organization: "other"}, Counts: counts},
	}

	s := buildSummary(results)
	assert.Equal(t, 2, s.Organizations["myorg"])
	assert.Equal(t, 1, s.Organizations["other"])
}

func TestBuildSummary_EmptyOrgSkipped(t *testing.T) {
	counts := &ResourceCounts{ByType: make(map[string]int), ByModule: make(map[string]int)}
	results := []WorkspaceResult{
		{Workspace: hcp.Workspace{}, Counts: counts}, // empty org
	}

	s := buildSummary(results)
	assert.Empty(t, s.Organizations)
}

func TestDefaultBatchConfig(t *testing.T) {
	cfg := DefaultBatchConfig()
	assert.Equal(t, 5, cfg.MaxWorkers)
	assert.Equal(t, 20, cfg.BatchSize)
}
