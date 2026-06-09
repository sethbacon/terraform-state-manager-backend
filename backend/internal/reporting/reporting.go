// Package reporting renders an analyzer.Analysis as a downloadable report in the
// same formats the terraform-state-analyzer CLI produces: JSON, Markdown, and CSV.
package reporting

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
)

// Format selects the report serialization.
type Format string

const (
	FormatJSON     Format = "json"
	FormatMarkdown Format = "md"
	FormatCSV      Format = "csv"
)

// Generate renders the analysis in the requested format, returning the MIME type,
// a suggested download filename, and the body bytes.
func Generate(a *analyzer.Analysis, key string, format Format) (contentType, filename string, body []byte, err error) {
	base := baseName(key)
	switch format {
	case FormatJSON:
		b, e := json.MarshalIndent(a, "", "  ")
		return "application/json", base + ".analysis.json", b, e
	case FormatMarkdown:
		return "text/markdown; charset=utf-8", base + ".analysis.md", markdown(a, key), nil
	case FormatCSV:
		b, e := csvReport(a)
		return "text/csv; charset=utf-8", base + ".analysis.csv", b, e
	default:
		return "", "", nil, fmt.Errorf("unsupported format %q (use json, md, or csv)", format)
	}
}

func baseName(key string) string {
	b := path.Base(key)
	b = strings.TrimSuffix(b, ".tfstate")
	if b == "" || b == "." || b == "/" {
		return "state"
	}
	return b
}

func markdown(a *analyzer.Analysis, key string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Terraform State Analysis — %s\n\n", key)
	fmt.Fprintf(&b, "- **Terraform version:** %s\n", orDash(a.TerraformVersion))
	fmt.Fprintf(&b, "- **State format version:** %d\n", a.FormatVersion)
	fmt.Fprintf(&b, "- **Serial:** %d\n", a.Serial)
	fmt.Fprintf(&b, "- **Lineage:** %s\n\n", orDash(a.Lineage))

	b.WriteString("## Summary\n\n")
	b.WriteString("| Metric | Value |\n|---|---:|\n")
	fmt.Fprintf(&b, "| Resources Under Management (RUM) | %d |\n", a.RUM)
	fmt.Fprintf(&b, "| Managed resources | %d |\n", a.ManagedResources)
	fmt.Fprintf(&b, "| Data sources | %d |\n", a.DataSources)
	fmt.Fprintf(&b, "| Total instances | %d |\n", a.TotalResources)
	fmt.Fprintf(&b, "| null_resource + terraform_data | %d |\n\n", a.NullResources)

	writeCountTable(&b, "Resource types", "Type", a.ResourceTypes)
	writeCountTable(&b, "Providers", "Provider", a.Providers)
	writeCountTable(&b, "Modules", "Module", a.Modules)
	return b.Bytes()
}

func writeCountTable(b *bytes.Buffer, title, keyHeader string, rows []analyzer.Count) {
	fmt.Fprintf(b, "## %s\n\n", title)
	if len(rows) == 0 {
		b.WriteString("_None_\n\n")
		return
	}
	fmt.Fprintf(b, "| %s | Count |\n|---|---:|\n", keyHeader)
	for _, r := range rows {
		fmt.Fprintf(b, "| %s | %d |\n", r.Key, r.Count)
	}
	b.WriteString("\n")
}

func csvReport(a *analyzer.Analysis) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write([]string{"category", "key", "count"}); err != nil {
		return nil, err
	}

	summary := [][2]any{
		{"rum", a.RUM},
		{"managed_resources", a.ManagedResources},
		{"data_sources", a.DataSources},
		{"total_instances", a.TotalResources},
		{"null_and_terraform_data", a.NullResources},
		{"terraform_version", a.TerraformVersion},
		{"serial", a.Serial},
	}
	for _, s := range summary {
		if err := w.Write([]string{"summary", fmt.Sprint(s[0]), fmt.Sprint(s[1])}); err != nil {
			return nil, err
		}
	}
	if err := writeCountRows(w, "resource_type", a.ResourceTypes); err != nil {
		return nil, err
	}
	if err := writeCountRows(w, "provider", a.Providers); err != nil {
		return nil, err
	}
	if err := writeCountRows(w, "module", a.Modules); err != nil {
		return nil, err
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCountRows(w *csv.Writer, category string, rows []analyzer.Count) error {
	for _, r := range rows {
		if err := w.Write([]string{category, r.Key, strconv.Itoa(r.Count)}); err != nil {
			return err
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
