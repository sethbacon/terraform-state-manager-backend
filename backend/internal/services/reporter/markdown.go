package reporter

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/services/analyzer"
)

// MarkdownReporter generates comprehensive Markdown reports from analysis output.
type MarkdownReporter struct{}

// Generate produces a Markdown report from the analysis output.
func (m *MarkdownReporter) Generate(output *analyzer.AnalysisOutput, _ string) ([]byte, string, error) {
	if output == nil {
		return nil, "", fmt.Errorf("analysis output is nil")
	}

	var b strings.Builder

	m.writeHeader(&b, output)
	m.writeSummary(&b, output)
	m.writeOrganizationBreakdown(&b, output)
	m.writeTopResourceTypes(&b, output)
	m.writeProviderAnalysis(&b, output)
	m.writeFailedWorkspaces(&b, output)
	m.writeWorkspaceDetails(&b, output)

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("terraform-state-report-%s.md", timestamp)

	return []byte(b.String()), filename, nil
}

// writeHeader writes the report title and generation timestamp.
func (m *MarkdownReporter) writeHeader(b *strings.Builder, output *analyzer.AnalysisOutput) {
	b.WriteString("# Terraform State Analysis Report\n\n")
	fmt.Fprintf(b, "**Generated:** %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(b, "**Analysis Duration:** %d ms\n\n", output.PerformanceMS)
	b.WriteString("---\n\n")
}

// writeSummary writes the high-level summary statistics section.
func (m *MarkdownReporter) writeSummary(b *strings.Builder, output *analyzer.AnalysisOutput) {
	b.WriteString("## Summary\n\n")

	summary := output.Summary
	if summary == nil {
		b.WriteString("No summary data available.\n\n")
		return
	}

	b.WriteString("| Metric | Value |\n")
	b.WriteString("|--------|-------|\n")
	fmt.Fprintf(b, "| **RUM (Resources Under Management)** | %d |\n", summary.TotalRUM)
	fmt.Fprintf(b, "| **Managed Resources** | %d |\n", summary.TotalManaged)
	fmt.Fprintf(b, "| **Total Resources** | %d |\n", summary.TotalResources)
	fmt.Fprintf(b, "| **Data Sources** | %d |\n", summary.TotalDataSources)
	fmt.Fprintf(b, "| **Total Workspaces** | %d |\n", summary.TotalWorkspaces)
	fmt.Fprintf(b, "| **Successful** | %d |\n", output.SuccessCount)
	fmt.Fprintf(b, "| **Failed** | %d |\n", output.FailCount)
	b.WriteString("\n")
}

// writeOrganizationBreakdown writes the organization breakdown table.
func (m *MarkdownReporter) writeOrganizationBreakdown(b *strings.Builder, output *analyzer.AnalysisOutput) {
	if output.Summary == nil || len(output.Summary.Organizations) == 0 {
		return
	}

	b.WriteString("## Organization Breakdown\n\n")
	b.WriteString("| Organization | Workspaces |\n")
	b.WriteString("|-------------|------------|\n")

	// Sort organizations by workspace count descending.
	type orgEntry struct {
		Name  string
		Count int
	}
	entries := make([]orgEntry, 0, len(output.Summary.Organizations))
	for name, count := range output.Summary.Organizations {
		entries = append(entries, orgEntry{Name: name, Count: count})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Count > entries[j].Count
	})

	for _, entry := range entries {
		fmt.Fprintf(b, "| %s | %d |\n", entry.Name, entry.Count)
	}
	b.WriteString("\n")
}

// writeTopResourceTypes writes the top 20 resource types table.
func (m *MarkdownReporter) writeTopResourceTypes(b *strings.Builder, output *analyzer.AnalysisOutput) {
	if output.Summary == nil || len(output.Summary.TopResourceTypes) == 0 {
		return
	}

	b.WriteString("## Top Resource Types (Top 20)\n\n")
	b.WriteString("| Resource Type | Count |\n")
	b.WriteString("|--------------|-------|\n")

	// Sort by count descending.
	type typeEntry struct {
		Type  string
		Count int
	}
	entries := make([]typeEntry, 0, len(output.Summary.TopResourceTypes))
	for typeName, count := range output.Summary.TopResourceTypes {
		entries = append(entries, typeEntry{Type: typeName, Count: count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count == entries[j].Count {
			return entries[i].Type < entries[j].Type
		}
		return entries[i].Count > entries[j].Count
	})

	// Limit to top 20.
	limit := 20
	if len(entries) < limit {
		limit = len(entries)
	}
	for _, entry := range entries[:limit] {
		fmt.Fprintf(b, "| `%s` | %d |\n", entry.Type, entry.Count)
	}

	if len(entries) > 20 {
		fmt.Fprintf(b, "\n*...and %d more resource types*\n", len(entries)-20)
	}
	b.WriteString("\n")
}

// writeProviderAnalysis writes the provider analysis table.
func (m *MarkdownReporter) writeProviderAnalysis(b *strings.Builder, output *analyzer.AnalysisOutput) {
	if output.Summary == nil || output.Summary.ProviderSummary == nil || len(output.Summary.ProviderSummary.AllProviders) == 0 {
		return
	}

	b.WriteString("## Provider Analysis\n\n")
	b.WriteString("| Provider | Resources | Workspaces Using | Resource Types |\n")
	b.WriteString("|----------|-----------|-----------------|----------------|\n")

	// Sort providers by resource count descending.
	type providerEntry struct {
		Name    string
		Summary *analyzer.ProviderUsageSummary
	}
	entries := make([]providerEntry, 0, len(output.Summary.ProviderSummary.AllProviders))
	for name, summary := range output.Summary.ProviderSummary.AllProviders {
		entries = append(entries, providerEntry{Name: name, Summary: summary})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Summary.TotalResources == entries[j].Summary.TotalResources {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Summary.TotalResources > entries[j].Summary.TotalResources
	})

	for _, entry := range entries {
		typeCount := len(entry.Summary.ResourceTypes)
		fmt.Fprintf(b, "| `%s` | %d | %d | %d |\n",
			entry.Name,
			entry.Summary.TotalResources,
			entry.Summary.WorkspacesUsing,
			typeCount,
		)
	}
	b.WriteString("\n")

	// Terraform version distribution.
	if len(output.Summary.ProviderSummary.TerraformVersions) > 0 {
		b.WriteString("### Terraform Versions\n\n")
		b.WriteString("| Version | Workspaces |\n")
		b.WriteString("|---------|------------|\n")

		type versionEntry struct {
			Version string
			Count   int
		}
		vEntries := make([]versionEntry, 0, len(output.Summary.ProviderSummary.TerraformVersions))
		for version, count := range output.Summary.ProviderSummary.TerraformVersions {
			vEntries = append(vEntries, versionEntry{Version: version, Count: count})
		}
		sort.Slice(vEntries, func(i, j int) bool {
			return vEntries[i].Count > vEntries[j].Count
		})

		for _, entry := range vEntries {
			fmt.Fprintf(b, "| %s | %d |\n", entry.Version, entry.Count)
		}
		b.WriteString("\n")
	}
}

// writeFailedWorkspaces writes the section listing workspaces that failed analysis.
func (m *MarkdownReporter) writeFailedWorkspaces(b *strings.Builder, output *analyzer.AnalysisOutput) {
	// Collect failed workspaces.
	var failed []analyzer.WorkspaceResult
	for _, r := range output.Results {
		if r.Error != nil {
			failed = append(failed, r)
		}
	}

	if len(failed) == 0 {
		return
	}

	b.WriteString("## Failed Workspaces\n\n")
	fmt.Fprintf(b, "**%d workspace(s) failed analysis:**\n\n", len(failed))
	b.WriteString("| Workspace | Organization | Error Type | Error |\n")
	b.WriteString("|-----------|-------------|------------|-------|\n")

	for _, r := range failed {
		org := r.Workspace.Organization
		if org == "" {
			org = "-"
		}
		errType := r.Error.ErrorType
		errMsg := r.Error.Message
		// Truncate long error messages for the table.
		if len(errMsg) > 80 {
			errMsg = errMsg[:77] + "..."
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			r.Workspace.Name, org, errType, errMsg)
	}
	b.WriteString("\n")
}

// writeWorkspaceDetails writes the individual workspace details section,
// sorted by RUM count descending.
func (m *MarkdownReporter) writeWorkspaceDetails(b *strings.Builder, output *analyzer.AnalysisOutput) {
	// Collect successful workspaces (those with counts).
	type wsDetail struct {
		Name         string
		Organization string
		Counts       *analyzer.ResourceCounts
	}

	var details []wsDetail
	for _, r := range output.Results {
		if r.Error != nil || r.Counts == nil {
			continue
		}
		org := r.Workspace.Organization
		if org == "" {
			org = "-"
		}
		details = append(details, wsDetail{
			Name:         r.Workspace.Name,
			Organization: org,
			Counts:       r.Counts,
		})
	}

	if len(details) == 0 {
		return
	}

	// Sort by RUM descending.
	sort.Slice(details, func(i, j int) bool {
		if details[i].Counts.RUM == details[j].Counts.RUM {
			return details[i].Name < details[j].Name
		}
		return details[i].Counts.RUM > details[j].Counts.RUM
	})

	b.WriteString("## Workspace Details\n\n")
	fmt.Fprintf(b, "*Showing %d workspace(s), sorted by RUM (descending)*\n\n", len(details))
	b.WriteString("| Workspace | Organization | RUM | Managed | Total | Data Sources | TF Version |\n")
	b.WriteString("|-----------|-------------|-----|---------|-------|-------------|------------|\n")

	for _, d := range details {
		tfVersion := d.Counts.TerraformVersion
		if tfVersion == "" {
			tfVersion = "-"
		}
		fmt.Fprintf(b, "| %s | %s | %d | %d | %d | %d | %s |\n",
			d.Name,
			d.Organization,
			d.Counts.RUM,
			d.Counts.Managed,
			d.Counts.Total,
			d.Counts.DataSources,
			tfVersion,
		)
	}
	b.WriteString("\n")

	b.WriteString("---\n\n")
	b.WriteString("*Report generated by Terraform State Manager*\n")
}
