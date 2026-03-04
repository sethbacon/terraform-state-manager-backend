package analyzer

import (
	"encoding/json"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// ParseState parses a raw state file JSON into ResourceCounts.
// Supports both v3+ format (resources array) and pre-v3 format (modules array).
func ParseState(stateData []byte) (*ResourceCounts, error) {
	if len(stateData) == 0 {
		return nil, fmt.Errorf("empty state data")
	}

	var stateFile hcp.StateFile
	if err := json.Unmarshal(stateData, &stateFile); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state file: %w", err)
	}

	// Determine the format by checking which fields are populated.
	// Modern v3+ format uses the resources array directly.
	// Legacy pre-0.12 format uses the modules array.
	var counts *ResourceCounts

	if len(stateFile.Resources) > 0 {
		// v3+ format: resources[] array present.
		counts = parseV3Resources(stateFile.Resources)
	} else if len(stateFile.Modules) > 0 {
		// Legacy pre-v3 format: modules[] array present.
		counts = parseLegacyModules(stateFile.Modules)
	} else {
		// Check if resources key exists but is empty vs not present at all.
		// Use a raw map to distinguish.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(stateData, &raw); err == nil {
			if _, hasResources := raw["resources"]; hasResources {
				// v3+ format but with empty resources.
				counts = NewResourceCounts()
			} else if _, hasModules := raw["modules"]; hasModules {
				// Legacy format but with empty modules.
				counts = NewResourceCounts()
			} else {
				// No resources or modules key at all.
				counts = NewResourceCounts()
			}
		} else {
			counts = NewResourceCounts()
		}
	}

	// Enrich counts with state file metadata.
	counts.TerraformVersion = stateFile.TerraformVersion
	counts.Serial = stateFile.Serial
	counts.Lineage = stateFile.Lineage

	// Extract provider analysis from the state file.
	counts.ProviderAnalysis = ExtractProviderAnalysis(&stateFile)

	return counts, nil
}

// parseV3Resources handles modern format with resources[] array.
// In v3+ state files, resources are at the top level with instances nested inside.
func parseV3Resources(resources []hcp.StateResource) *ResourceCounts {
	return CountResources(resources)
}

// parseLegacyModules handles pre-0.12 format with modules[] array.
// Legacy state files organize resources under modules with a path hierarchy.
func parseLegacyModules(modules []hcp.StateModule) *ResourceCounts {
	counts := NewResourceCounts()

	for _, mod := range modules {
		// Determine module path name.
		moduleName := "root"
		if len(mod.Path) > 1 {
			// Join path segments beyond the initial "root" segment.
			moduleName = ""
			for i, segment := range mod.Path {
				if i > 0 {
					if moduleName != "" {
						moduleName += "."
					}
					moduleName += segment
				}
			}
			if moduleName == "" {
				moduleName = "root"
			}
		}

		for _, resource := range mod.Resources {
			// Each legacy resource entry counts as one instance.
			counts.Total++
			counts.Managed++
			counts.ByModule[moduleName]++

			resourceType := resource.Type
			if resourceType != "" {
				counts.ByType[resourceType]++

				if ExcludedResourceTypes[resourceType] {
					counts.ExcludedNull++
				}
			}
		}
	}

	// Calculate RUM.
	counts.RUM = CalculateRUM(counts.Managed, counts.ExcludedNull)

	return counts
}
