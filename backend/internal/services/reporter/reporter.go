// Package reporter implements report generation in multiple formats (markdown,
// JSON, CSV) from analysis output data.
package reporter

import (
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/services/analyzer"
)

// ReportGenerator is the interface that format-specific generators implement.
type ReportGenerator interface {
	// Generate produces report bytes from analysis output.
	// Returns the report data, a suggested filename, and any error.
	Generate(output *analyzer.AnalysisOutput, format string) ([]byte, string, error)
}

// supportedFormats maps format names to their generator constructors.
var supportedFormats = map[string]func() ReportGenerator{
	"markdown": func() ReportGenerator { return &MarkdownReporter{} },
	"json":     func() ReportGenerator { return &JSONReporter{} },
	"csv":      func() ReportGenerator { return &CSVReporter{} },
}

// GenerateReport picks the appropriate generator for the given format and
// produces the report. Returns the report bytes, suggested filename, and error.
func GenerateReport(output *analyzer.AnalysisOutput, format string) ([]byte, string, error) {
	factory, ok := supportedFormats[format]
	if !ok {
		return nil, "", fmt.Errorf("unsupported report format: %s (supported: markdown, json, csv)", format)
	}

	generator := factory()
	return generator.Generate(output, format)
}

// SupportedFormats returns the list of supported report format names.
func SupportedFormats() []string {
	formats := make([]string, 0, len(supportedFormats))
	for f := range supportedFormats {
		formats = append(formats, f)
	}
	return formats
}

// IsValidFormat returns true if the given format string is a supported report format.
func IsValidFormat(format string) bool {
	_, ok := supportedFormats[format]
	return ok
}
