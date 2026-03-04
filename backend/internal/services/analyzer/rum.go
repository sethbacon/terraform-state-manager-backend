package analyzer

// ExcludedResourceTypes defines resource types excluded from RUM counts.
// RUM = managed_count - excluded_count.
// Excluded types: null_resource, terraform_data.
var ExcludedResourceTypes = map[string]bool{
	"null_resource":  true,
	"terraform_data": true,
}

// ResourceCounts holds the counted resources from a state file.
type ResourceCounts struct {
	Total            int               `json:"total"`
	Managed          int               `json:"managed"`
	RUM              int               `json:"rum"`
	ExcludedNull     int               `json:"excluded_null"`
	DataSources      int               `json:"data_sources"`
	ByType           map[string]int    `json:"by_type"`
	ByModule         map[string]int    `json:"by_module"`
	ProviderAnalysis *ProviderAnalysis `json:"provider_analysis,omitempty"`
	TerraformVersion string            `json:"terraform_version,omitempty"`
	Serial           int               `json:"serial"`
	Lineage          string            `json:"lineage,omitempty"`
	LastModified     string            `json:"last_modified,omitempty"`
}

// NewResourceCounts creates a ResourceCounts with initialized maps.
func NewResourceCounts() *ResourceCounts {
	return &ResourceCounts{
		ByType:   make(map[string]int),
		ByModule: make(map[string]int),
	}
}

// CalculateRUM computes RUM (Resources Under Management) from managed and excluded counts.
// RUM = managed - excludedNull.
func CalculateRUM(managed, excludedNull int) int {
	rum := managed - excludedNull
	if rum < 0 {
		return 0
	}
	return rum
}
