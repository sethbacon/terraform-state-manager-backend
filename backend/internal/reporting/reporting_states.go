// reporting_states.go renders a *set* of analyzed state files (the Reports
// page's filtered fleet view) as a downloadable report. It is the multi-state
// sibling of Generate, which renders a single analyzer.Analysis. The input is a
// neutral StateRecord so this package stays decoupled from the db layer.
package reporting

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// StateRecord is one analyzed state file in a fleet report: scalar metrics plus
// the provider and resource-type maps. The handler maps its persisted rows onto
// this type so reporting needn't import the repository layer.
type StateRecord struct {
	SourceName       string         `json:"source_name"`
	SourceType       string         `json:"source_type"`
	StateKey         string         `json:"state_key"`
	TerraformVersion string         `json:"terraform_version"`
	Serial           int64          `json:"serial"`
	Lineage          string         `json:"lineage"`
	Size             int64          `json:"size"`
	RUM              int            `json:"rum"`
	ManagedResources int            `json:"managed_resources"`
	DataSources      int            `json:"data_sources"`
	TotalResources   int            `json:"total_resources"`
	Providers        map[string]int `json:"providers,omitempty"`
	ResourceTypes    map[string]int `json:"resource_types,omitempty"`
	AnalyzedAt       string         `json:"analyzed_at"`
}

// StatesReportMeta carries context for the report header: when it was generated,
// the applied filters as a structured object (for JSON) and a human-readable
// rendering (for Markdown).
type StatesReportMeta struct {
	GeneratedAt string         `json:"generated_at"`
	Filters     map[string]any `json:"filters,omitempty"`
	FilterText  string         `json:"-"`
}

// StatesSummary are the totals across the matched states, shown in every format
// so a reader can sanity-check the slice without re-summing the rows.
type StatesSummary struct {
	Matched          int `json:"matched"`
	RUM              int `json:"rum"`
	ManagedResources int `json:"managed_resources"`
	DataSources      int `json:"data_sources"`
	TotalResources   int `json:"total_resources"`
}

// SummarizeStates totals the metrics across the records. Exported so the live
// preview endpoint can return the same summary the export embeds.
func SummarizeStates(records []StateRecord) StatesSummary {
	s := StatesSummary{Matched: len(records)}
	for _, r := range records {
		s.RUM += r.RUM
		s.ManagedResources += r.ManagedResources
		s.DataSources += r.DataSources
		s.TotalResources += r.TotalResources
	}
	return s
}

// GenerateStatesReport renders the matched states in the requested format,
// returning the MIME type, a suggested download filename, and the body bytes.
func GenerateStatesReport(records []StateRecord, meta StatesReportMeta, format Format) (contentType, filename string, body []byte, err error) {
	switch format {
	case FormatJSON:
		b, e := json.MarshalIndent(statesReportJSON{
			GeneratedAt: meta.GeneratedAt,
			Filters:     meta.Filters,
			Summary:     SummarizeStates(records),
			States:      records,
		}, "", "  ")
		return "application/json", "terraform-state-report.json", b, e
	case FormatMarkdown:
		return "text/markdown; charset=utf-8", "terraform-state-report.md", statesMarkdown(records, meta), nil
	case FormatCSV:
		b, e := statesCSV(records)
		return "text/csv; charset=utf-8", "terraform-state-report.csv", b, e
	default:
		return "", "", nil, fmt.Errorf("unsupported format %q (use json, md, or csv)", format)
	}
}

type statesReportJSON struct {
	GeneratedAt string         `json:"generated_at"`
	Filters     map[string]any `json:"filters,omitempty"`
	Summary     StatesSummary  `json:"summary"`
	States      []StateRecord  `json:"states"`
}

func statesCSV(records []StateRecord) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	header := []string{
		"source_name", "source_type", "state_key", "terraform_version", "serial", "size",
		"rum", "managed_resources", "data_sources", "total_resources",
		"providers", "resource_types", "lineage", "analyzed_at",
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}
	for _, r := range records {
		row := []string{
			r.SourceName, r.SourceType, r.StateKey, r.TerraformVersion,
			strconv.FormatInt(r.Serial, 10), strconv.FormatInt(r.Size, 10),
			strconv.Itoa(r.RUM), strconv.Itoa(r.ManagedResources),
			strconv.Itoa(r.DataSources), strconv.Itoa(r.TotalResources),
			joinCountMap(r.Providers), joinCountMap(r.ResourceTypes),
			r.Lineage, r.AnalyzedAt,
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func statesMarkdown(records []StateRecord, meta StatesReportMeta) []byte {
	var b bytes.Buffer
	b.WriteString("# Terraform State Report\n\n")
	if meta.GeneratedAt != "" {
		fmt.Fprintf(&b, "_Generated %s_\n\n", meta.GeneratedAt)
	}
	fmt.Fprintf(&b, "**Filters:** %s\n\n", orDash(meta.FilterText))

	s := SummarizeStates(records)
	b.WriteString("## Summary\n\n")
	b.WriteString("| Metric | Value |\n|---|---:|\n")
	fmt.Fprintf(&b, "| Matched state files | %d |\n", s.Matched)
	fmt.Fprintf(&b, "| Resources Under Management (RUM) | %d |\n", s.RUM)
	fmt.Fprintf(&b, "| Managed resources | %d |\n", s.ManagedResources)
	fmt.Fprintf(&b, "| Data sources | %d |\n", s.DataSources)
	fmt.Fprintf(&b, "| Total instances | %d |\n\n", s.TotalResources)

	b.WriteString("## State files\n\n")
	if len(records) == 0 {
		b.WriteString("_None_\n")
		return b.Bytes()
	}
	b.WriteString("| Source | State | Terraform | RUM | Managed | Data | Total | Size |\n")
	b.WriteString("|---|---|---|---:|---:|---:|---:|---:|\n")
	for _, r := range records {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %d |\n",
			mdCell(r.SourceName), mdCell(r.StateKey), mdCell(orDash(r.TerraformVersion)),
			r.RUM, r.ManagedResources, r.DataSources, r.TotalResources, r.Size)
	}
	return b.Bytes()
}

// joinCountMap renders a name->count map as a stable "name=count;name=count"
// string so the JSONB maps survive flattening into a single CSV cell.
func joinCountMap(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(m[k]))
	}
	return strings.Join(parts, ";")
}

// mdCell escapes the pipe character so a value can't break the Markdown table.
func mdCell(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
