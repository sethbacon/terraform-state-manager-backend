package analyzer

import (
	"sort"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// ProviderAnalysis holds detailed provider analysis results.
type ProviderAnalysis struct {
	ProviderVersions   map[string]string         `json:"provider_versions"`
	ProviderUsage      map[string]*ProviderUsage `json:"provider_usage"`
	ProviderStatistics *ProviderStatistics       `json:"provider_statistics"`
	VersionAnalysis    *VersionAnalysis          `json:"version_analysis,omitempty"`
}

// ProviderUsage tracks how a single provider is used across resources.
type ProviderUsage struct {
	ResourceCount int      `json:"resource_count"`
	ResourceTypes []string `json:"resource_types"`
	Modules       []string `json:"modules"`
}

// ProviderStatistics holds aggregate provider statistics.
type ProviderStatistics struct {
	TotalProviders           int `json:"total_providers"`
	ProvidersWithVersions    int `json:"providers_with_versions"`
	ProvidersWithoutVersions int `json:"providers_without_versions"`
	TotalProviderResources   int `json:"total_provider_resources"`
}

// VersionAnalysis holds version information from the state file.
type VersionAnalysis struct {
	TerraformVersion string `json:"terraform_version"`
	StateVersion     int    `json:"state_version"`
}

// providerPrefixMap maps resource type prefixes to provider names.
var providerPrefixMap = map[string]string{
	"aws_":        "hashicorp/aws",
	"azurerm_":    "hashicorp/azurerm",
	"azuread_":    "hashicorp/azuread",
	"google_":     "hashicorp/google",
	"kubernetes_": "hashicorp/kubernetes",
	"helm_":       "hashicorp/helm",
	"null_":       "hashicorp/null",
	"random_":     "hashicorp/random",
	"datadog_":    "datadog/datadog",
	"github_":     "integrations/github",
	"gitlab_":     "gitlabhq/gitlab",
	"cloudflare_": "cloudflare/cloudflare",
	"vault_":      "hashicorp/vault",
	"tls_":        "hashicorp/tls",
	"local_":      "hashicorp/local",
	"template_":   "hashicorp/template",
	"external_":   "hashicorp/external",
	"archive_":    "hashicorp/archive",
	"dns_":        "hashicorp/dns",
	"http_":       "hashicorp/http",
	"time_":       "hashicorp/time",
}

// NormalizeProviderName normalizes provider strings to a consistent format.
// Handles formats: registry.terraform.io/hashicorp/aws, hashicorp/aws, aws.
func NormalizeProviderName(providerString string) string {
	if providerString == "" {
		return "unknown"
	}

	provider := strings.TrimSpace(providerString)

	// Handle full registry path: registry.terraform.io/hashicorp/aws
	// or provider["registry.terraform.io/hashicorp/aws"]
	provider = strings.TrimPrefix(provider, "provider[\"")
	provider = strings.TrimSuffix(provider, "\"]")

	// Split on slashes to extract the meaningful parts.
	parts := strings.Split(provider, "/")

	switch len(parts) {
	case 1:
		// Simple name like "aws" - assume hashicorp namespace.
		return "hashicorp/" + parts[0]
	case 2:
		// Already namespace/name format like "hashicorp/aws".
		return parts[0] + "/" + parts[1]
	case 3:
		// Full registry path: registry.terraform.io/hashicorp/aws
		return parts[1] + "/" + parts[2]
	default:
		// Take last two segments if there are more than 3.
		if len(parts) >= 2 {
			return parts[len(parts)-2] + "/" + parts[len(parts)-1]
		}
		return provider
	}
}

// InferProviderFromResourceType guesses the provider from a resource type prefix.
func InferProviderFromResourceType(resourceType string) string {
	// Sort prefixes by length descending so longer prefixes match first
	// (e.g., "azurerm_" before "azure_").
	type prefixEntry struct {
		prefix   string
		provider string
	}
	entries := make([]prefixEntry, 0, len(providerPrefixMap))
	for prefix, provider := range providerPrefixMap {
		entries = append(entries, prefixEntry{prefix: prefix, provider: provider})
	}
	sort.Slice(entries, func(i, j int) bool {
		return len(entries[i].prefix) > len(entries[j].prefix)
	})

	for _, entry := range entries {
		if strings.HasPrefix(resourceType, entry.prefix) {
			return entry.provider
		}
	}

	// Fall back to extracting the first segment before underscore.
	idx := strings.Index(resourceType, "_")
	if idx > 0 {
		return "hashicorp/" + resourceType[:idx]
	}

	return "unknown"
}

// ExtractProviderAnalysis analyzes providers from state file resources.
func ExtractProviderAnalysis(stateFile *hcp.StateFile) *ProviderAnalysis {
	if stateFile == nil {
		return &ProviderAnalysis{
			ProviderVersions:   make(map[string]string),
			ProviderUsage:      make(map[string]*ProviderUsage),
			ProviderStatistics: &ProviderStatistics{},
		}
	}

	providerVersions := make(map[string]string)
	providerUsage := make(map[string]*ProviderUsage)

	// Track unique resource types and modules per provider.
	providerResourceTypes := make(map[string]map[string]bool)
	providerModules := make(map[string]map[string]bool)

	for _, resource := range stateFile.Resources {
		// Determine the provider name.
		var providerName string
		if resource.Provider != "" {
			providerName = NormalizeProviderName(resource.Provider)
		} else {
			providerName = InferProviderFromResourceType(resource.Type)
		}

		instanceCount := len(resource.Instances)
		if instanceCount == 0 {
			instanceCount = 1
		}

		// Initialize provider usage tracking if needed.
		if _, exists := providerUsage[providerName]; !exists {
			providerUsage[providerName] = &ProviderUsage{}
			providerResourceTypes[providerName] = make(map[string]bool)
			providerModules[providerName] = make(map[string]bool)
		}

		providerUsage[providerName].ResourceCount += instanceCount
		providerResourceTypes[providerName][resource.Type] = true

		module := resource.Module
		if module == "" {
			module = "root"
		}
		providerModules[providerName][module] = true
	}

	// Convert sets to sorted slices for deterministic output.
	for providerName, usage := range providerUsage {
		if types, ok := providerResourceTypes[providerName]; ok {
			typeSlice := make([]string, 0, len(types))
			for t := range types {
				typeSlice = append(typeSlice, t)
			}
			sort.Strings(typeSlice)
			usage.ResourceTypes = typeSlice
		}
		if modules, ok := providerModules[providerName]; ok {
			modSlice := make([]string, 0, len(modules))
			for m := range modules {
				modSlice = append(modSlice, m)
			}
			sort.Strings(modSlice)
			usage.Modules = modSlice
		}
	}

	// Build statistics.
	totalProviderResources := 0
	for _, usage := range providerUsage {
		totalProviderResources += usage.ResourceCount
	}

	stats := &ProviderStatistics{
		TotalProviders:           len(providerUsage),
		ProvidersWithVersions:    len(providerVersions),
		ProvidersWithoutVersions: len(providerUsage) - len(providerVersions),
		TotalProviderResources:   totalProviderResources,
	}

	analysis := &ProviderAnalysis{
		ProviderVersions:   providerVersions,
		ProviderUsage:      providerUsage,
		ProviderStatistics: stats,
	}

	// Add version analysis if terraform version is available.
	if stateFile.TerraformVersion != "" {
		analysis.VersionAnalysis = &VersionAnalysis{
			TerraformVersion: stateFile.TerraformVersion,
			StateVersion:     stateFile.Version,
		}
	}

	return analysis
}
