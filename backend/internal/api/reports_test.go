package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
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

// allStatesCols mirrors the AllStates query projection so the mock rows line up
// with the repository's Scan order.
var allStatesCols = []string{
	"source_id", "name", "type", "state_key", "terraform_version", "serial", "lineage", "size",
	"rum", "managed_resources", "data_sources", "total_resources", "providers", "resource_types", "analyzed_at",
}

func allStatesRows() *sqlmock.Rows {
	return sqlmock.NewRows(allStatesCols).
		AddRow("s1", "prod", "s3", "envs/prod/app.tfstate", "1.5.7", 10, "lin-1", 2048,
			40, 38, 2, 42, []byte(`{"registry.terraform.io/hashicorp/aws":40}`), []byte(`{"aws_instance":12}`), "2026-06-18T00:00:00Z").
		AddRow("s2", "dev", "local", "dev/net.tfstate", "0.14.11", 5, "lin-2", 512,
			8, 8, 0, 8, []byte(`{"registry.terraform.io/hashicorp/azurerm":8}`), []byte(`{"azurerm_virtual_network":3}`), "2026-06-18T00:00:00Z")
}

// reportEnv wires the two Reports routes over a sqlmock app DB.
func reportEnv(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := NewSourcesHandlers(db, nil)
	r := gin.New()
	r.GET("/api/v1/reports/states", h.ReportStates())
	r.GET("/api/v1/reports/states/export", h.ReportStatesExport())
	return r, mock
}

func doGet(r *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestReportStatesHandler(t *testing.T) {
	t.Run("returns summary and rows for the full match", func(t *testing.T) {
		r, mock := reportEnv(t)
		mock.ExpectQuery("FROM state_analyses a").WillReturnRows(allStatesRows())

		w := doGet(r, "/api/v1/reports/states")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Total     int  `json:"total"`
			Truncated bool `json:"truncated"`
			Summary   struct {
				Matched, RUM int
			} `json:"summary"`
			States []reportPreviewRow `json:"states"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Total != 2 || resp.Truncated || resp.Summary.Matched != 2 || resp.Summary.RUM != 48 {
			t.Errorf("unexpected response: %+v", resp)
		}
		if len(resp.States) != 2 || resp.States[0].SourceName != "prod" || resp.States[0].StateKey != "envs/prod/app.tfstate" {
			t.Errorf("unexpected rows: %+v", resp.States)
		}
	})

	t.Run("applies a filter (version range) over the query path", func(t *testing.T) {
		r, mock := reportEnv(t)
		mock.ExpectQuery("FROM state_analyses a").WillReturnRows(allStatesRows())

		w := doGet(r, "/api/v1/reports/states?version=1.0.0&op=lt")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"total":1`) || !strings.Contains(w.Body.String(), "dev/net.tfstate") {
			t.Errorf("expected only the <1.0.0 state: %s", w.Body.String())
		}
	})

	t.Run("rejects a bad filter", func(t *testing.T) {
		r, _ := reportEnv(t)
		w := doGet(r, "/api/v1/reports/states?rum_min=abc")
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("surfaces a store error", func(t *testing.T) {
		r, mock := reportEnv(t)
		mock.ExpectQuery("FROM state_analyses a").WillReturnError(errors.New("store down"))
		w := doGet(r, "/api/v1/reports/states")
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
	})
}

func TestReportStatesExportHandler(t *testing.T) {
	t.Run("downloads each format with filters described", func(t *testing.T) {
		for _, tc := range []struct {
			format, contentType, ext string
		}{
			{"csv", "text/csv", ".csv"},
			{"json", "application/json", ".json"},
			{"md", "text/markdown", ".md"},
		} {
			r, mock := reportEnv(t)
			mock.ExpectQuery("FROM state_analyses a").WillReturnRows(allStatesRows())

			w := doGet(r, "/api/v1/reports/states/export?format="+tc.format+"&provider=aws&rum_min=1")
			if w.Code != http.StatusOK {
				t.Fatalf("%s: status = %d (%s)", tc.format, w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, tc.contentType) {
				t.Errorf("%s: content-type = %q", tc.format, ct)
			}
			if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "terraform-state-report"+tc.ext) {
				t.Errorf("%s: disposition = %q", tc.format, cd)
			}
			if w.Body.Len() == 0 {
				t.Errorf("%s: empty body", tc.format)
			}
		}
	})

	t.Run("defaults to json", func(t *testing.T) {
		r, mock := reportEnv(t)
		mock.ExpectQuery("FROM state_analyses a").WillReturnRows(allStatesRows())
		w := doGet(r, "/api/v1/reports/states/export")
		if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Errorf("default format: status = %d ct = %q", w.Code, w.Header().Get("Content-Type"))
		}
	})

	t.Run("rejects an unknown format", func(t *testing.T) {
		r, mock := reportEnv(t)
		mock.ExpectQuery("FROM state_analyses a").WillReturnRows(allStatesRows())
		w := doGet(r, "/api/v1/reports/states/export?format=xml")
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("rejects a bad filter before loading", func(t *testing.T) {
		r, _ := reportEnv(t)
		w := doGet(r, "/api/v1/reports/states/export?size_max=nope")
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("surfaces a store error", func(t *testing.T) {
		r, mock := reportEnv(t)
		mock.ExpectQuery("FROM state_analyses a").WillReturnError(errors.New("store down"))
		w := doGet(r, "/api/v1/reports/states/export?format=csv")
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
	})
}
