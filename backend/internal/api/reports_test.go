package api

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

func reportSampleRows() []repositories.StateRow {
	return []repositories.StateRow{
		{SourceID: "s1", SourceName: "prod", SourceType: "s3", StateKey: "envs/prod/app.tfstate",
			TerraformVersion: "1.5.7", Serial: 10, Size: 2048, RUM: 40, ManagedResources: 38, DataSources: 2, TotalResources: 42,
			Providers: map[string]int{"registry.terraform.io/hashicorp/aws": 40}, ResourceTypes: map[string]int{"aws_instance": 12}},
		{SourceID: "s1", SourceName: "prod", SourceType: "s3", StateKey: "envs/prod/net.tfstate",
			TerraformVersion: "0.14.11", Serial: 5, Size: 512, RUM: 8, ManagedResources: 8, DataSources: 0, TotalResources: 8,
			Providers: map[string]int{"registry.terraform.io/hashicorp/azurerm": 8}, ResourceTypes: map[string]int{"azurerm_virtual_network": 3}},
		{SourceID: "s2", SourceName: "dev", SourceType: "local", StateKey: "dev/sandbox.tfstate",
			TerraformVersion: "", Serial: 1, Size: 64, RUM: 0, ManagedResources: 0, DataSources: 0, TotalResources: 0,
			Providers: map[string]int{}, ResourceTypes: map[string]int{}},
		{SourceID: "s2", SourceName: "dev", SourceType: "local", StateKey: "dev/data.tfstate",
			TerraformVersion: "1.9.5", Serial: 3, Size: 9000, RUM: 5, ManagedResources: 2, DataSources: 3, TotalResources: 5,
			Providers: map[string]int{"registry.terraform.io/hashicorp/aws": 5}, ResourceTypes: map[string]int{"aws_s3_bucket": 1}},
	}
}

// widest is a reportFilters with the no-op numeric bounds parseReportFilters
// applies, so a test case only sets the predicate it exercises.
func widest() reportFilters {
	return reportFilters{
		rumMax: math.MaxInt, managedMax: math.MaxInt, dataMax: math.MaxInt,
		totalMax: math.MaxInt, sizeMax: math.MaxInt64,
	}
}

func matchedKeys(rows []repositories.StateRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.StateKey)
	}
	return out
}

func eqKeys(got []repositories.StateRow, want []string) bool {
	keys := matchedKeys(got)
	if len(keys) != len(want) {
		return false
	}
	for i := range want {
		if keys[i] != want[i] {
			return false
		}
	}
	return true
}

func TestApplyReportFilters(t *testing.T) {
	rows := reportSampleRows()

	withKey := func(mut func(*reportFilters)) reportFilters {
		f := widest()
		mut(&f)
		return f
	}

	cases := []struct {
		name string
		f    reportFilters
		want []string
	}{
		{"no filters", widest(),
			[]string{"envs/prod/app.tfstate", "envs/prod/net.tfstate", "dev/sandbox.tfstate", "dev/data.tfstate"}},
		{"by source", withKey(func(f *reportFilters) { f.sourceIDs = []string{"s2"} }),
			[]string{"dev/sandbox.tfstate", "dev/data.tfstate"}},
		{"key substring", withKey(func(f *reportFilters) { f.keyQuery = "prod" }),
			[]string{"envs/prod/app.tfstate", "envs/prod/net.tfstate"}},
		{"version lt 1.0.0", withKey(func(f *reportFilters) { f.version, f.versionOp = "1.0.0", "lt" }),
			[]string{"envs/prod/net.tfstate"}},
		{"version eq exact", withKey(func(f *reportFilters) { f.version, f.versionOp = "1.5.7", "eq" }),
			[]string{"envs/prod/app.tfstate"}},
		{"version unknown", withKey(func(f *reportFilters) { f.version, f.versionOp = "unknown", "eq" }),
			[]string{"dev/sandbox.tfstate"}},
		{"provider aws", withKey(func(f *reportFilters) { f.provider = "aws" }),
			[]string{"envs/prod/app.tfstate", "dev/data.tfstate"}},
		{"resource type aws_", withKey(func(f *reportFilters) { f.resourceType = "aws_" }),
			[]string{"envs/prod/app.tfstate", "dev/data.tfstate"}},
		{"rum min 10", withKey(func(f *reportFilters) { f.rumMin = 10 }),
			[]string{"envs/prod/app.tfstate"}},
		{"rum max 5", withKey(func(f *reportFilters) { f.rumMax = 5 }),
			[]string{"dev/sandbox.tfstate", "dev/data.tfstate"}},
		{"size min 1000", withKey(func(f *reportFilters) { f.sizeMin = 1000 }),
			[]string{"envs/prod/app.tfstate", "dev/data.tfstate"}},
		{"source + provider", withKey(func(f *reportFilters) { f.sourceIDs, f.provider = []string{"s1"}, "aws" }),
			[]string{"envs/prod/app.tfstate"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyReportFilters(rows, tc.f)
			if !eqKeys(got, tc.want) {
				t.Errorf("applyReportFilters = %v, want %v", matchedKeys(got), tc.want)
			}
		})
	}
}

func reportCtx(rawQuery string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/?"+rawQuery, nil)
	return c
}

func TestParseReportFilters(t *testing.T) {
	t.Run("defaults are widest", func(t *testing.T) {
		f, err := parseReportFilters(reportCtx(""))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if f.rumMin != 0 || f.rumMax != math.MaxInt || f.version != "" || len(f.sourceIDs) != 0 {
			t.Errorf("unexpected defaults: %+v", f)
		}
	})

	t.Run("parses values", func(t *testing.T) {
		f, err := parseReportFilters(reportCtx("source_id=a&source_id=b&q=Prod&rum_min=10&rum_max=100&version=1.0.0"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(f.sourceIDs) != 2 || f.keyQuery != "prod" || f.rumMin != 10 || f.rumMax != 100 {
			t.Errorf("unexpected parse: %+v", f)
		}
		if f.version != "1.0.0" || f.versionOp != "eq" { // op defaults to eq when a version is given
			t.Errorf("version op default = %q", f.versionOp)
		}
	})

	t.Run("rejects bad number", func(t *testing.T) {
		if _, err := parseReportFilters(reportCtx("rum_min=abc")); err == nil {
			t.Error("expected error for non-numeric rum_min")
		}
	})

	t.Run("rejects bad op", func(t *testing.T) {
		if _, err := parseReportFilters(reportCtx("version=1.0.0&op=between")); err == nil {
			t.Error("expected error for invalid op")
		}
	})
}

func TestReportFiltersDescribe(t *testing.T) {
	f := widest()
	f.version, f.versionOp, f.provider, f.rumMin = "1.0.0", "lt", "aws", 10

	m, text := f.describe()
	if m["version"] != "1.0.0" || m["version_op"] != "lt" || m["provider"] != "aws" || m["rum_min"] != 10 {
		t.Errorf("describe map = %+v", m)
	}
	for _, want := range []string{"version < 1.0.0", "provider", "rum ≥ 10"} {
		if !strings.Contains(text, want) {
			t.Errorf("describe text %q missing %q", text, want)
		}
	}
}
