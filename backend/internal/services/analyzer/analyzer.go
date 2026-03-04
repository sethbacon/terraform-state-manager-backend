package analyzer

import (
	"context"
	"sort"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
)

// Analyzer orchestrates the full analysis pipeline.
type Analyzer struct {
	hcpClient *hcp.Client
	batchCfg  BatchConfig
}

// NewAnalyzer creates a new Analyzer with the given HCP client and batch configuration.
func NewAnalyzer(hcpClient *hcp.Client, batchCfg BatchConfig) *Analyzer {
	return &Analyzer{
		hcpClient: hcpClient,
		batchCfg:  batchCfg,
	}
}

// AnalysisOutput holds the complete output of an analysis run.
type AnalysisOutput struct {
	Results         []WorkspaceResult `json:"results"`
	Summary         *AnalysisSummary  `json:"summary"`
	PerformanceMS   int64             `json:"performance_ms"`
	TotalWorkspaces int               `json:"total_workspaces"`
	SuccessCount    int               `json:"success_count"`
	FailCount       int               `json:"fail_count"`
}

// AnalysisSummary aggregates analysis across all workspaces.
type AnalysisSummary struct {
	TotalRUM         int                  `json:"total_rum"`
	TotalManaged     int                  `json:"total_managed"`
	TotalResources   int                  `json:"total_resources"`
	TotalDataSources int                  `json:"total_data_sources"`
	TotalWorkspaces  int                  `json:"total_workspaces"`
	Organizations    map[string]int       `json:"organizations"`
	TopResourceTypes map[string]int       `json:"top_resource_types"`
	ProviderSummary  *ProviderSummaryData `json:"provider_summary,omitempty"`
}

// ProviderSummaryData aggregates provider analysis across all workspaces.
type ProviderSummaryData struct {
	AllProviders      map[string]*ProviderUsageSummary `json:"all_providers"`
	ProviderVersions  map[string]map[string]int        `json:"provider_versions"`
	TerraformVersions map[string]int                   `json:"terraform_versions"`
}

// ProviderUsageSummary summarizes provider usage across workspaces.
type ProviderUsageSummary struct {
	TotalResources  int      `json:"total_resources"`
	WorkspacesUsing int      `json:"workspaces_using"`
	ResourceTypes   []string `json:"resource_types"`
}

// AnalyzeHCPTerraform runs analysis against HCP Terraform workspaces.
// It fetches workspaces, downloads their state files, parses them, and summarizes the results.
func (a *Analyzer) AnalyzeHCPTerraform(ctx context.Context, orgFilter string) (*AnalysisOutput, error) {
	startTime := time.Now()

	// Get workspaces from HCP client.
	workspaces, err := a.hcpClient.GetAllWorkspaces(ctx, orgFilter)
	if err != nil {
		return nil, &AnalysisError{
			ErrorType: ErrorTypeException,
			Message:   "failed to list workspaces from HCP",
			Err:       err,
		}
	}

	if len(workspaces) == 0 {
		elapsed := time.Since(startTime).Milliseconds()
		return &AnalysisOutput{
			Results:         []WorkspaceResult{},
			Summary:         newEmptySummary(),
			PerformanceMS:   elapsed,
			TotalWorkspaces: 0,
			SuccessCount:    0,
			FailCount:       0,
		}, nil
	}

	// Process workspaces in batches with concurrency control.
	results, err := ProcessWorkspacesInBatches(ctx, a.hcpClient, workspaces, a.batchCfg)
	if err != nil {
		return nil, &AnalysisError{
			ErrorType: ErrorTypeException,
			Message:   "batch processing failed",
			Err:       err,
		}
	}

	// Build summary from results.
	successCount := 0
	failCount := 0
	for _, r := range results {
		if r.Error != nil {
			failCount++
		} else {
			successCount++
		}
	}

	summary := buildSummary(results)
	elapsed := time.Since(startTime).Milliseconds()

	return &AnalysisOutput{
		Results:         results,
		Summary:         summary,
		PerformanceMS:   elapsed,
		TotalWorkspaces: len(workspaces),
		SuccessCount:    successCount,
		FailCount:       failCount,
	}, nil
}

// buildSummary aggregates results from all workspace analyses into a summary.
func buildSummary(results []WorkspaceResult) *AnalysisSummary {
	summary := &AnalysisSummary{
		Organizations:    make(map[string]int),
		TopResourceTypes: make(map[string]int),
	}

	// Provider summary aggregation state.
	allProviders := make(map[string]*ProviderUsageSummary)
	providerVersions := make(map[string]map[string]int)
	terraformVersions := make(map[string]int)

	// Track resource types per provider across workspaces.
	providerResourceTypes := make(map[string]map[string]bool)

	for _, result := range results {
		if result.Error != nil {
			continue
		}

		counts := result.Counts
		if counts == nil {
			continue
		}

		// Aggregate top-level counts.
		summary.TotalRUM += counts.RUM
		summary.TotalManaged += counts.Managed
		summary.TotalResources += counts.Total
		summary.TotalDataSources += counts.DataSources
		summary.TotalWorkspaces++

		// Track organizations.
		org := result.Workspace.Organization
		if org != "" {
			summary.Organizations[org]++
		}

		// Aggregate resource types.
		for resourceType, count := range counts.ByType {
			summary.TopResourceTypes[resourceType] += count
		}

		// Aggregate provider analysis.
		if counts.ProviderAnalysis != nil {
			for providerName, usage := range counts.ProviderAnalysis.ProviderUsage {
				if _, exists := allProviders[providerName]; !exists {
					allProviders[providerName] = &ProviderUsageSummary{}
					providerResourceTypes[providerName] = make(map[string]bool)
				}

				allProviders[providerName].TotalResources += usage.ResourceCount
				allProviders[providerName].WorkspacesUsing++

				for _, rt := range usage.ResourceTypes {
					providerResourceTypes[providerName][rt] = true
				}
			}

			// Aggregate provider versions.
			for provider, version := range counts.ProviderAnalysis.ProviderVersions {
				if _, exists := providerVersions[provider]; !exists {
					providerVersions[provider] = make(map[string]int)
				}
				providerVersions[provider][version]++
			}

			// Track terraform versions.
			if counts.ProviderAnalysis.VersionAnalysis != nil && counts.ProviderAnalysis.VersionAnalysis.TerraformVersion != "" {
				terraformVersions[counts.ProviderAnalysis.VersionAnalysis.TerraformVersion]++
			}
		}
	}

	// Finalize provider resource types into sorted slices.
	for providerName, pSummary := range allProviders {
		if types, ok := providerResourceTypes[providerName]; ok {
			typeSlice := make([]string, 0, len(types))
			for t := range types {
				typeSlice = append(typeSlice, t)
			}
			sort.Strings(typeSlice)
			pSummary.ResourceTypes = typeSlice
		}
	}

	// Only attach provider summary if there's data.
	if len(allProviders) > 0 {
		summary.ProviderSummary = &ProviderSummaryData{
			AllProviders:      allProviders,
			ProviderVersions:  providerVersions,
			TerraformVersions: terraformVersions,
		}
	}

	return summary
}

// newEmptySummary creates an empty AnalysisSummary.
func newEmptySummary() *AnalysisSummary {
	return &AnalysisSummary{
		Organizations:    make(map[string]int),
		TopResourceTypes: make(map[string]int),
	}
}
