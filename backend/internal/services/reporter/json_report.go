package reporter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/services/analyzer"
)

// JSONReporter generates a JSON report containing the full analysis output.
type JSONReporter struct{}

// Generate produces a formatted JSON report from the analysis output.
func (j *JSONReporter) Generate(output *analyzer.AnalysisOutput, _ string) ([]byte, string, error) {
	if output == nil {
		return nil, "", fmt.Errorf("analysis output is nil")
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal analysis output to JSON: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("terraform-state-report-%s.json", timestamp)

	return data, filename, nil
}
