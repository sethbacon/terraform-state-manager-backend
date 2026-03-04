package analyzer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

func TestExtractWorkspaceMetadata(t *testing.T) {
	ws := hcp.Workspace{
		ID:               "ws-001",
		Name:             "production",
		Organization:     "myorg",
		TerraformVersion: "1.5.0",
		CreatedAt:        "2024-01-01T00:00:00Z",
		UpdatedAt:        "2024-06-01T00:00:00Z",
	}

	meta := ExtractWorkspaceMetadata(ws)
	require.NotNil(t, meta)
	assert.Equal(t, "ws-001", meta.WorkspaceID)
	assert.Equal(t, "production", meta.WorkspaceName)
	assert.Equal(t, "myorg", meta.Organization)
	assert.Equal(t, "1.5.0", meta.TerraformVersion)
	assert.Equal(t, "2024-01-01T00:00:00Z", meta.CreatedAt)
	assert.Equal(t, "2024-06-01T00:00:00Z", meta.UpdatedAt)
}

func TestExtractWorkspaceMetadata_EmptyWorkspace(t *testing.T) {
	meta := ExtractWorkspaceMetadata(hcp.Workspace{})
	require.NotNil(t, meta)
	assert.Empty(t, meta.WorkspaceID)
	assert.Empty(t, meta.Organization)
}

func TestMergeMetadata_NilCounts_CreatesNew(t *testing.T) {
	sf := &hcp.StateFile{TerraformVersion: "1.4.0", Serial: 5, Lineage: "test-lineage"}
	result := MergeMetadata(nil, nil, sf)
	require.NotNil(t, result)
	assert.Equal(t, "1.4.0", result.TerraformVersion)
	assert.Equal(t, 5, result.Serial)
	assert.Equal(t, "test-lineage", result.Lineage)
}

func TestMergeMetadata_StateFileApplied(t *testing.T) {
	counts := NewResourceCounts()
	sf := &hcp.StateFile{TerraformVersion: "1.5.0", Serial: 10, Lineage: "line-abc"}

	result := MergeMetadata(counts, nil, sf)
	require.NotNil(t, result)
	assert.Equal(t, "1.5.0", result.TerraformVersion)
	assert.Equal(t, 10, result.Serial)
	assert.Equal(t, "line-abc", result.Lineage)
}

func TestMergeMetadata_WorkspaceOverridesStateFile(t *testing.T) {
	counts := NewResourceCounts()
	sf := &hcp.StateFile{TerraformVersion: "1.4.0", Serial: 3}
	wsMeta := &WorkspaceMetadata{
		TerraformVersion: "1.5.0",
		StateSerial:      7,
		UpdatedAt:        "2024-06-01",
	}

	result := MergeMetadata(counts, wsMeta, sf)
	require.NotNil(t, result)
	// Workspace version takes priority
	assert.Equal(t, "1.5.0", result.TerraformVersion)
	assert.Equal(t, 7, result.Serial)
	assert.Equal(t, "2024-06-01", result.LastModified)
}

func TestMergeMetadata_ExistingVersionNotOverwrittenByStateFile(t *testing.T) {
	counts := NewResourceCounts()
	counts.TerraformVersion = "1.6.0"  // already set
	counts.Serial = 99                 // already set

	sf := &hcp.StateFile{TerraformVersion: "1.4.0", Serial: 1}

	result := MergeMetadata(counts, nil, sf)
	// Pre-existing values should NOT be overwritten by state file
	assert.Equal(t, "1.6.0", result.TerraformVersion)
	assert.Equal(t, 99, result.Serial)
}

func TestMergeMetadata_NilStateFile(t *testing.T) {
	counts := NewResourceCounts()
	result := MergeMetadata(counts, nil, nil)
	require.NotNil(t, result)
	// No panic, empty counts returned intact
	assert.Equal(t, 0, result.Total)
}

func TestMergeMetadata_ProviderAnalysisPopulatedFromStateFile(t *testing.T) {
	counts := NewResourceCounts()
	sf := &hcp.StateFile{
		TerraformVersion: "1.5.0",
		Resources: []hcp.StateResource{
			{
				Mode:      "managed",
				Type:      "aws_instance",
				Provider:  "provider[\"registry.terraform.io/hashicorp/aws\"]",
				Instances: []hcp.StateInstance{{}},
			},
		},
	}

	result := MergeMetadata(counts, nil, sf)
	require.NotNil(t, result)
	require.NotNil(t, result.ProviderAnalysis)
	assert.Contains(t, result.ProviderAnalysis.ProviderUsage, "hashicorp/aws")
}
