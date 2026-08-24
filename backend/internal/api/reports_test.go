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

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"

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

// TestApplyResidual covers the predicates that stay in Go (semver-range version,
// provider/resource-type substring). The clean predicates now live in SQL and
// are covered by TestSQLFilterMapping + the repository's buildStateWhere test.
func TestApplyResidual(t *testing.T) {
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
		{"no residual passes through unchanged", widest(),
			[]string{"envs/prod/app.tfstate", "envs/prod/net.tfstate", "dev/sandbox.tfstate", "dev/data.tfstate"}},
		{"version lt 1.0.0", withKey(func(f *reportFilters) { f.version, f.versionOp = "1.0.0", "lt" }),
			[]string{"envs/prod/net.tfstate"}},
		{"provider aws", withKey(func(f *reportFilters) { f.provider = "aws" }),
			[]string{"envs/prod/app.tfstate", "dev/data.tfstate"}},
		{"resource type aws_", withKey(func(f *reportFilters) { f.resourceType = "aws_" }),
			[]string{"envs/prod/app.tfstate", "dev/data.tfstate"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.applyResidual(rows)
			if !eqKeys(got, tc.want) {
				t.Errorf("applyResidual = %v, want %v", matchedKeys(got), tc.want)
			}
		})
	}
}

// TestSQLFilterMapping asserts the handler projects the clean predicates onto the
// repository filter (and leaves the residual ones out), and that hasResidual
// classifies each filter correctly.
func TestSQLFilterMapping(t *testing.T) {
	t.Run("clean predicates map; residual stays out", func(t *testing.T) {
		f := widest()
		f.sourceIDs = []string{"s1", "s2"}
		f.keyQuery = "prod"
		f.version, f.versionOp = "1.5.7", "eq"
		f.rumMin = 10
		f.sizeMax = 1000
		f.provider = "aws" // residual — must NOT appear in the SQL filter

		q := f.sqlFilter()
		if len(q.SourceIDs) != 2 || q.KeyContains != "prod" {
			t.Errorf("source/key not mapped: %+v", q)
		}
		if q.VersionExact == nil || *q.VersionExact != "1.5.7" {
			t.Errorf("version eq not mapped: %+v", q.VersionExact)
		}
		if q.RUMMin == nil || *q.RUMMin != 10 || q.RUMMax != nil {
			t.Errorf("rum bounds: min=%v max=%v", q.RUMMin, q.RUMMax)
		}
		if q.SizeMax == nil || *q.SizeMax != 1000 || q.SizeMin != nil {
			t.Errorf("size bounds: min=%v max=%v", q.SizeMin, q.SizeMax)
		}
	})

	t.Run("unknown version maps to the empty string", func(t *testing.T) {
		f := widest()
		f.version, f.versionOp = "unknown", "eq"
		q := f.sqlFilter()
		if q.VersionExact == nil || *q.VersionExact != "" {
			t.Errorf("unknown should map to \"\": %+v", q.VersionExact)
		}
	})

	t.Run("semver-range version is residual, not projected", func(t *testing.T) {
		f := widest()
		f.version, f.versionOp = "1.0.0", "lt"
		if q := f.sqlFilter(); q.VersionExact != nil {
			t.Errorf("range version must not project to VersionExact: %+v", q.VersionExact)
		}
		if !f.hasResidual() {
			t.Error("range version should be residual")
		}
	})

	t.Run("hasResidual classification", func(t *testing.T) {
		if widest().hasResidual() {
			t.Error("empty filter has no residual")
		}
		cleanOnly := widest()
		cleanOnly.sourceIDs, cleanOnly.keyQuery, cleanOnly.rumMin = []string{"s1"}, "k", 5
		cleanOnly.version, cleanOnly.versionOp = "1.5.7", "eq"
		if cleanOnly.hasResidual() {
			t.Error("source/key/eq-version/numeric are all clean")
		}
	})
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

// previewCols mirrors the PreviewStatesWithTotals projection (scalar columns +
// the four window aggregates), so mock rows line up with the repository's Scan.
var previewCols = []string{
	"source_id", "name", "type", "state_key", "terraform_version", "serial", "size",
	"rum", "managed_resources", "data_sources", "total_resources", "analyzed_at",
	"full_count", "sum_rum", "sum_managed", "sum_data", "sum_total",
}

// previewRows returns the same two states as allStatesRows in the preview shape.
// The window aggregates repeat on every row (COUNT=2, and the per-column SUMs).
func previewRows() *sqlmock.Rows {
	return sqlmock.NewRows(previewCols).
		AddRow("s1", "prod", "s3", "envs/prod/app.tfstate", "1.5.7", 10, 2048,
			40, 38, 2, 42, "2026-06-18T00:00:00Z", 2, 48, 46, 2, 50).
		AddRow("s2", "dev", "local", "dev/net.tfstate", "0.14.11", 5, 512,
			8, 8, 0, 8, "2026-06-18T00:00:00Z", 2, 48, 46, 2, 50)
}

// reportEnv wires the two Reports routes over a sqlmock app DB.
func reportEnv(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	// newSQLMock, not sqlmock.New: the scoped report statements bind
	// scope.OrgIDs as a []string to `= ANY($1::uuid[])`, which the default
	// converter rejects at the driver before any expectation is consulted.
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := NewSourcesHandlers(db, nil)
	r := gin.New()
	// What middleware.TenantScope publishes in production (#459). Stored rather
	// than resolved, so this rig needs no membership store — but it must be
	// stored, because these routes now treat an unresolved scope as a wiring
	// fault and answer 500 rather than reading the whole store.
	r.Use(func(c *gin.Context) {
		tenantscope.Store(c, tenantscope.Scope{OrgIDs: []string{testActingOrg}})
		c.Next()
	})
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
		// No filter → no residual → the window-aggregate preview query.
		mock.ExpectQuery("FROM state_analyses a").WillReturnRows(previewRows())

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

	t.Run("reports truncation from the SQL count on the fast path", func(t *testing.T) {
		r, mock := reportEnv(t)
		// full_count exceeds the preview cap even though only two rows are returned:
		// the window COUNT reflects the whole match, so truncated must be true.
		big := sqlmock.NewRows(previewCols).
			AddRow("s1", "prod", "s3", "envs/prod/app.tfstate", "1.5.7", 10, 2048,
				40, 38, 2, 42, "2026-06-18T00:00:00Z", reportPreviewCap+5, 48, 46, 2, 50).
			AddRow("s2", "dev", "local", "dev/net.tfstate", "0.14.11", 5, 512,
				8, 8, 0, 8, "2026-06-18T00:00:00Z", reportPreviewCap+5, 48, 46, 2, 50)
		mock.ExpectQuery("FROM state_analyses a").WillReturnRows(big)

		w := doGet(r, "/api/v1/reports/states")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"truncated":true`) ||
			!strings.Contains(w.Body.String(), `"total":505`) {
			t.Errorf("expected truncated total: %s", w.Body.String())
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
