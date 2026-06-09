package dashboards

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/analyzer"
)

const testOrgID = "11111111-1111-1111-1111-111111111111"

// newTestRouter wires the dashboards handlers over a sqlmock DB with an
// org-injecting middleware standing in for the auth chain. providerSrc supplies
// the (stubbed) provider-version source for pin-drift; pass nil to disable it.
func newTestRouter(t *testing.T, providerSrc analyzer.ProviderVersionSource) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	runRepo := repositories.NewAnalysisRunRepository(db)
	resultRepo := repositories.NewAnalysisResultRepository(db)
	h := NewHandlers(db, runRepo, resultRepo, providerSrc)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("organization_id", testOrgID)
		c.Next()
	})
	r.GET("/dashboard/version-drift", h.GetVersionDrift)
	return r, mock
}

// expectNoProviderPins mocks the provider-lock-pins query returning no rows, for
// tests that only exercise the Terraform-version-drift path.
func expectNoProviderPins(mock sqlmock.Sqlmock, runID string) {
	mock.ExpectQuery(`SELECT provider_lock_pins\s+FROM analysis_results\s+WHERE run_id = \$1 AND status = \$2 AND provider_lock_pins IS NOT NULL`).
		WithArgs(runID, "success").
		WillReturnRows(sqlmock.NewRows([]string{"provider_lock_pins"}))
}

// expectLatestCompletedRun mocks getLatestCompletedRun returning one run.
func expectLatestCompletedRun(mock sqlmock.Sqlmock, runID string) {
	cols := []string{
		"id", "organization_id", "source_id", "status", "trigger_type", "config",
		"started_at", "completed_at", "total_workspaces", "successful_count", "failed_count",
		"total_rum", "total_managed", "total_resources", "total_data_sources",
		"error_message", "performance_ms", "triggered_by", "created_at", "updated_at",
	}
	now := time.Now()
	rows := sqlmock.NewRows(cols).AddRow(
		runID, testOrgID, nil, "completed", "manual", []byte(`{}`),
		now, now, 2, 2, 0,
		10, 10, 12, 2,
		nil, nil, nil, now, now,
	)
	mock.ExpectQuery(`SELECT id, organization_id, source_id, status, trigger_type, config.*FROM analysis_runs\s+WHERE organization_id = \$1 AND status = \$2`).
		WithArgs(testOrgID, "completed").
		WillReturnRows(rows)
}

func doGet(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGetVersionDrift_AggregatesEntries(t *testing.T) {
	r, mock := newTestRouter(t, nil)
	runID := "22222222-2222-2222-2222-222222222222"

	expectLatestCompletedRun(mock, runID)

	mock.ExpectQuery(`SELECT workspace_name, version_drift_report\s+FROM analysis_results\s+WHERE run_id = \$1 AND status = \$2 AND version_drift_report IS NOT NULL`).
		WithArgs(runID, "success").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_name", "version_drift_report"}).
			AddRow("ws-satisfied", []byte(`{"required":">= 1.5.0","actual":"1.7.0","satisfies":true,"status":"satisfied"}`)).
			AddRow("ws-drift", []byte(`{"required":">= 1.5.0","actual":"1.4.0","satisfies":false,"status":"drift"}`)).
			AddRow("ws-unknown", []byte(`{"required":">= 1.5.0","actual":"","satisfies":false,"status":"unknown"}`)),
		)
	expectNoProviderPins(mock, runID)

	w := doGet(t, r, "/dashboard/version-drift")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Data VersionDriftResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, runID, resp.Data.RunID)
	require.Equal(t, 3, resp.Data.Total)
	require.Equal(t, 1, resp.Data.Satisfied)
	require.Equal(t, 1, resp.Data.Drift)
	require.Equal(t, 1, resp.Data.Unknown)
	require.Len(t, resp.Data.Entries, 3)
	require.Equal(t, "ws-drift", resp.Data.Entries[1].WorkspaceName)
	require.False(t, resp.Data.Entries[1].Satisfies)
	// Provider-pin section present but empty when no lock pins recorded.
	require.Equal(t, 0, resp.Data.ProviderPins.Total)
	require.Empty(t, resp.Data.ProviderPins.Entries)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetVersionDrift_NoCompletedRun(t *testing.T) {
	r, mock := newTestRouter(t, nil)

	mock.ExpectQuery(`SELECT id, organization_id, source_id, status, trigger_type, config.*FROM analysis_runs\s+WHERE organization_id = \$1 AND status = \$2`).
		WithArgs(testOrgID, "completed").
		WillReturnError(sql.ErrNoRows)

	w := doGet(t, r, "/dashboard/version-drift")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Data    VersionDriftResponse `json:"data"`
		Message string               `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "no completed analysis runs found", resp.Message)
	require.Empty(t, resp.Data.Entries)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetVersionDrift_NoReports(t *testing.T) {
	r, mock := newTestRouter(t, nil)
	runID := "33333333-3333-3333-3333-333333333333"

	expectLatestCompletedRun(mock, runID)

	mock.ExpectQuery(`SELECT workspace_name, version_drift_report\s+FROM analysis_results`).
		WithArgs(runID, "success").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_name", "version_drift_report"}))
	expectNoProviderPins(mock, runID)

	w := doGet(t, r, "/dashboard/version-drift")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Data VersionDriftResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Data.Total)
	require.Empty(t, resp.Data.Entries)
	require.Empty(t, resp.Data.ProviderPins.Entries)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestGetVersionDrift_ProviderPins exercises the additive provider-pin section:
// a stub registry source drives up_to_date / behind / unknown classifications
// and constraint-satisfaction, alongside the existing TF-version-drift entries.
func TestGetVersionDrift_ProviderPins(t *testing.T) {
	src := &analyzer.StaticProviderVersionSource{
		Versions: map[string][]string{
			"hashicorp/aws":    {"5.30.0", "5.31.0", "5.40.0"}, // latest 5.40.0
			"hashicorp/random": {"3.6.0"},                      // latest equals pin
		},
		Unknown: map[string]bool{
			"hashicorp/null": true, // unknown provider
		},
	}
	r, mock := newTestRouter(t, src)
	runID := "44444444-4444-4444-4444-444444444444"

	expectLatestCompletedRun(mock, runID)

	mock.ExpectQuery(`SELECT workspace_name, version_drift_report\s+FROM analysis_results\s+WHERE run_id = \$1 AND status = \$2 AND version_drift_report IS NOT NULL`).
		WithArgs(runID, "success").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_name", "version_drift_report"}).
			AddRow("ws", []byte(`{"required":">= 1.5.0","actual":"1.7.0","satisfies":true,"status":"satisfied"}`)),
		)

	pins := []byte(`[
		{"source":"registry.terraform.io/hashicorp/aws","version":"5.31.0","constraints":">= 5.0.0, < 6.0.0"},
		{"source":"registry.terraform.io/hashicorp/random","version":"3.6.0","constraints":"~> 3.0"},
		{"source":"registry.terraform.io/hashicorp/null","version":"3.2.0","constraints":"~> 3.0"}
	]`)
	mock.ExpectQuery(`SELECT provider_lock_pins\s+FROM analysis_results\s+WHERE run_id = \$1 AND status = \$2 AND provider_lock_pins IS NOT NULL`).
		WithArgs(runID, "success").
		WillReturnRows(sqlmock.NewRows([]string{"provider_lock_pins"}).AddRow(pins))

	w := doGet(t, r, "/dashboard/version-drift")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Data VersionDriftResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// TF-version-drift fields are unchanged by the provider-pin enrichment.
	require.Equal(t, 1, resp.Data.Total)
	require.Equal(t, 1, resp.Data.Satisfied)

	pp := resp.Data.ProviderPins
	require.Equal(t, 3, pp.Total)
	require.Equal(t, 1, pp.UpToDate) // random
	require.Equal(t, 1, pp.Behind)   // aws
	require.Equal(t, 1, pp.Unknown)  // null
	require.Len(t, pp.Entries, 3)

	// Entries are sorted by source address: aws, null, random.
	aws := pp.Entries[0]
	require.Equal(t, "registry.terraform.io/hashicorp/aws", aws.Source)
	require.Equal(t, "5.31.0", aws.Pinned)
	require.Equal(t, "5.40.0", aws.LatestAvailable)
	require.Equal(t, "behind", aws.Status)
	require.True(t, aws.SatisfiesConstraint)

	null := pp.Entries[1]
	require.Equal(t, "registry.terraform.io/hashicorp/null", null.Source)
	require.Equal(t, "unknown", null.Status)
	require.Empty(t, null.LatestAvailable)

	random := pp.Entries[2]
	require.Equal(t, "registry.terraform.io/hashicorp/random", random.Source)
	require.Equal(t, "up_to_date", random.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}
