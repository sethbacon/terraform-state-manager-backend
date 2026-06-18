package reporting

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleRecords() []StateRecord {
	return []StateRecord{
		{SourceName: "prod", SourceType: "s3", StateKey: "envs/prod/app.tfstate", TerraformVersion: "1.5.7",
			Serial: 10, Size: 2048, RUM: 40, ManagedResources: 38, DataSources: 2, TotalResources: 42,
			Providers: map[string]int{"hashicorp/aws": 40}, ResourceTypes: map[string]int{"aws_instance": 12},
			AnalyzedAt: "2026-06-18T00:00:00Z"},
		{SourceName: "dev", SourceType: "local", StateKey: "dev/data.tfstate", TerraformVersion: "1.9.5",
			Serial: 3, Size: 9000, RUM: 5, ManagedResources: 2, DataSources: 3, TotalResources: 5,
			AnalyzedAt: "2026-06-18T00:00:00Z"},
	}
}

func TestGenerateStatesReportFormats(t *testing.T) {
	meta := StatesReportMeta{GeneratedAt: "2026-06-18T12:00:00Z", FilterText: "version < 2.0.0"}
	for _, f := range []Format{FormatJSON, FormatMarkdown, FormatCSV} {
		ct, fn, body, err := GenerateStatesReport(sampleRecords(), meta, f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if ct == "" || len(body) == 0 {
			t.Fatalf("%s: empty content type or body", f)
		}
		if !strings.HasPrefix(fn, "terraform-state-report.") {
			t.Errorf("%s: unexpected filename %q", f, fn)
		}
	}
}

func TestStatesCSV(t *testing.T) {
	_, _, body, err := GenerateStatesReport(sampleRecords(), StatesReportMeta{}, FormatCSV)
	if err != nil {
		t.Fatal(err)
	}
	csv := string(body)
	if !strings.HasPrefix(csv, "source_name,source_type,state_key,terraform_version,serial,size,") {
		t.Errorf("csv missing/!= header:\n%s", csv)
	}
	if !strings.Contains(csv, "hashicorp/aws=40") {
		t.Errorf("csv missing flattened providers cell:\n%s", csv)
	}
	if !strings.Contains(csv, "envs/prod/app.tfstate") {
		t.Errorf("csv missing state row:\n%s", csv)
	}
}

func TestStatesJSON(t *testing.T) {
	_, _, body, err := GenerateStatesReport(sampleRecords(),
		StatesReportMeta{GeneratedAt: "t0", Filters: map[string]any{"version": "2.0.0", "version_op": "lt"}}, FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		GeneratedAt string         `json:"generated_at"`
		Summary     StatesSummary  `json:"summary"`
		States      []StateRecord  `json:"states"`
		Filters     map[string]any `json:"filters"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.GeneratedAt != "t0" || out.Filters["version"] != "2.0.0" {
		t.Errorf("meta = %+v / %+v", out.GeneratedAt, out.Filters)
	}
	if out.Summary.Matched != 2 || out.Summary.RUM != 45 || len(out.States) != 2 {
		t.Errorf("summary = %+v, states = %d", out.Summary, len(out.States))
	}
}

func TestStatesMarkdownEscapesPipes(t *testing.T) {
	recs := []StateRecord{{SourceName: "a|b", StateKey: "k|x", TerraformVersion: "1.0.0"}}
	_, _, body, err := GenerateStatesReport(recs, StatesReportMeta{FilterText: "none"}, FormatMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	md := string(body)
	if !strings.Contains(md, "## Summary") || !strings.Contains(md, "## State files") {
		t.Errorf("markdown missing sections:\n%s", md)
	}
	if !strings.Contains(md, `a\|b`) || !strings.Contains(md, `k\|x`) {
		t.Errorf("markdown did not escape pipes:\n%s", md)
	}
}

func TestSummarizeStates(t *testing.T) {
	s := SummarizeStates(sampleRecords())
	if s.Matched != 2 || s.RUM != 45 || s.ManagedResources != 40 || s.DataSources != 5 || s.TotalResources != 47 {
		t.Errorf("summary = %+v", s)
	}
}

func TestJoinCountMap(t *testing.T) {
	if got := joinCountMap(map[string]int{"b": 2, "a": 1}); got != "a=1;b=2" {
		t.Errorf("joinCountMap sorted = %q", got)
	}
	if got := joinCountMap(nil); got != "" {
		t.Errorf("joinCountMap(nil) = %q", got)
	}
}

func TestGenerateStatesReportUnknownFormat(t *testing.T) {
	if _, _, _, err := GenerateStatesReport(nil, StatesReportMeta{}, Format("xml")); err == nil {
		t.Error("expected error for unknown format")
	}
}
