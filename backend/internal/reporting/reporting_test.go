package reporting

import (
	"strings"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
)

func sample() *analyzer.Analysis {
	return &analyzer.Analysis{
		TerraformVersion: "1.7.5",
		Serial:           3,
		RUM:              3,
		ManagedResources: 5,
		DataSources:      1,
		TotalResources:   6,
		NullResources:    2,
		ResourceTypes:    []analyzer.Count{{Key: "aws_instance", Count: 2}},
		Providers:        []analyzer.Count{{Key: "hashicorp/aws", Count: 2}},
		Modules:          []analyzer.Count{{Key: "root", Count: 5}},
	}
}

func TestGenerateFormats(t *testing.T) {
	for _, f := range []Format{FormatJSON, FormatMarkdown, FormatCSV} {
		ct, fn, body, err := Generate(sample(), "envs/prod.tfstate", f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if ct == "" || len(body) == 0 {
			t.Fatalf("%s: empty content type or body", f)
		}
		if !strings.HasPrefix(fn, "prod.analysis.") {
			t.Errorf("%s: unexpected filename %q", f, fn)
		}
	}

	md := mustBody(t, FormatMarkdown)
	if !strings.Contains(md, "Resources Under Management (RUM) | 3") {
		t.Errorf("markdown missing RUM summary:\n%s", md)
	}
	csv := mustBody(t, FormatCSV)
	if !strings.Contains(csv, "summary,rum,3") || !strings.Contains(csv, "resource_type,aws_instance,2") {
		t.Errorf("csv missing expected rows:\n%s", csv)
	}
}

func TestGenerateUnknownFormat(t *testing.T) {
	if _, _, _, err := Generate(sample(), "k", Format("xml")); err == nil {
		t.Error("expected error for unknown format")
	}
}

func mustBody(t *testing.T, f Format) string {
	t.Helper()
	_, _, body, err := Generate(sample(), "prod.tfstate", f)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
