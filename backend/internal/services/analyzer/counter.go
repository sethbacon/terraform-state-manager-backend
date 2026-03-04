package analyzer

import (
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// CountResources counts resources from a state file resources array.
// This is the core counting logic ported from Python's _count_resources_from_state.
// It counts instances (len(resource.Instances)), not resource blocks.
func CountResources(resources []hcp.StateResource) *ResourceCounts {
	counts := NewResourceCounts()

	for _, resource := range resources {
		mode := resource.Mode
		if mode == "" {
			mode = "managed"
		}

		resourceType := resource.Type
		instanceCount := len(resource.Instances)

		// Count all instances toward total regardless of mode.
		counts.Total += instanceCount

		// Determine module for by_module tracking.
		module := resource.Module
		if module == "" {
			module = "root"
		}
		counts.ByModule[module] += instanceCount

		switch mode {
		case "managed":
			counts.Managed += instanceCount

			// Track by resource type.
			counts.ByType[resourceType] += instanceCount

			// Check if this resource type is excluded from RUM.
			if ExcludedResourceTypes[resourceType] {
				counts.ExcludedNull += instanceCount
			}

		case "data":
			counts.DataSources += instanceCount
		}
	}

	// Calculate RUM: managed minus excluded.
	counts.RUM = CalculateRUM(counts.Managed, counts.ExcludedNull)

	return counts
}
