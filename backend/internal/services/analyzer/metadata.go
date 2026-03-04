package analyzer

import (
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// WorkspaceMetadata holds metadata about a workspace from HCP or other sources.
type WorkspaceMetadata struct {
	WorkspaceID      string
	WorkspaceName    string
	Organization     string
	TerraformVersion string
	CreatedAt        string
	UpdatedAt        string
	StateSerial      int
	StateDownloadURL string
}

// MergeMetadata combines state file metadata with workspace metadata.
// Priority: HCP metadata > workspace metadata > state file.
// This ensures the most authoritative source of truth takes precedence.
func MergeMetadata(counts *ResourceCounts, wsMeta *WorkspaceMetadata, stateFile *hcp.StateFile) *ResourceCounts {
	if counts == nil {
		counts = NewResourceCounts()
	}

	// Apply state file metadata first (lowest priority).
	if stateFile != nil {
		if counts.TerraformVersion == "" {
			counts.TerraformVersion = stateFile.TerraformVersion
		}
		if counts.Serial == 0 {
			counts.Serial = stateFile.Serial
		}
		if counts.Lineage == "" {
			counts.Lineage = stateFile.Lineage
		}
	}

	// Apply workspace metadata (medium priority), overriding state file values.
	if wsMeta != nil {
		if wsMeta.TerraformVersion != "" {
			counts.TerraformVersion = wsMeta.TerraformVersion
		}
		if wsMeta.StateSerial > 0 {
			counts.Serial = wsMeta.StateSerial
		}
		if wsMeta.UpdatedAt != "" {
			counts.LastModified = wsMeta.UpdatedAt
		}
	}

	// Provider analysis comes from the parsed state file.
	if stateFile != nil && counts.ProviderAnalysis == nil {
		counts.ProviderAnalysis = ExtractProviderAnalysis(stateFile)
	}

	return counts
}

// ExtractWorkspaceMetadata creates a WorkspaceMetadata from an HCP Workspace.
func ExtractWorkspaceMetadata(ws hcp.Workspace) *WorkspaceMetadata {
	return &WorkspaceMetadata{
		WorkspaceID:      ws.ID,
		WorkspaceName:    ws.Name,
		Organization:     ws.Organization,
		TerraformVersion: ws.TerraformVersion,
		CreatedAt:        ws.CreatedAt,
		UpdatedAt:        ws.UpdatedAt,
	}
}
