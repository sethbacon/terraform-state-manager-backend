package reporter

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/services/analyzer"
)

// CSVReporter generates a CSV report with one row per workspace.
type CSVReporter struct{}

// Generate produces a CSV report from the analysis output.
// Columns: workspace_name, organization, status, total_resources, managed, rum,
// data_sources, terraform_version.
func (c *CSVReporter) Generate(output *analyzer.AnalysisOutput, _ string) ([]byte, string, error) {
	if output == nil {
		return nil, "", fmt.Errorf("analysis output is nil")
	}

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)

	// Write header row.
	header := []string{
		"workspace_name",
		"organization",
		"status",
		"total_resources",
		"managed",
		"rum",
		"data_sources",
		"terraform_version",
	}
	if err := writer.Write(header); err != nil {
		return nil, "", fmt.Errorf("failed to write CSV header: %w", err)
	}

	// Write one row per workspace result.
	for _, result := range output.Results {
		status := "success"
		if result.Error != nil {
			status = "failed"
		}

		org := result.Workspace.Organization
		if org == "" {
			org = ""
		}

		var (
			totalResources = 0
			managed        = 0
			rum            = 0
			dataSources    = 0
			tfVersion      = ""
		)

		if result.Counts != nil {
			totalResources = result.Counts.Total
			managed = result.Counts.Managed
			rum = result.Counts.RUM
			dataSources = result.Counts.DataSources
			tfVersion = result.Counts.TerraformVersion
		}

		row := []string{
			result.Workspace.Name,
			org,
			status,
			fmt.Sprintf("%d", totalResources),
			fmt.Sprintf("%d", managed),
			fmt.Sprintf("%d", rum),
			fmt.Sprintf("%d", dataSources),
			tfVersion,
		}
		if err := writer.Write(row); err != nil {
			return nil, "", fmt.Errorf("failed to write CSV row for workspace %s: %w", result.Workspace.Name, err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, "", fmt.Errorf("failed to flush CSV writer: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("terraform-state-report-%s.csv", timestamp)

	return buf.Bytes(), filename, nil
}
