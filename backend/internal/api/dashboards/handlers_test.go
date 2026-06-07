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
)

const testOrgID = "11111111-1111-1111-1111-111111111111"

// newTestRouter wires the dashboards handlers over a sqlmock DB with an
// org-injecting middleware standing in for the auth chain.
func newTestRouter(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	runRepo := repositories.NewAnalysisRunRepository(db)
	resultRepo := repositories.NewAnalysisResultRepository(db)
	h := NewHandlers(db, runRepo, resultRepo)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("organization_id", testOrgID)
		c.Next()
	})
	r.GET("/dashboard/version-drift", h.GetVersionDrift)
	return r, mock
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
	r, mock := newTestRouter(t)
	runID := "22222222-2222-2222-2222-222222222222"

	expectLatestCompletedRun(mock, runID)

	mock.ExpectQuery(`SELECT workspace_name, version_drift_report\s+FROM analysis_results\s+WHERE run_id = \$1 AND status = \$2 AND version_drift_report IS NOT NULL`).
		WithArgs(runID, "success").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_name", "version_drift_report"}).
			AddRow("ws-satisfied", []byte(`{"required":">= 1.5.0","actual":"1.7.0","satisfies":true,"status":"satisfied"}`)).
			AddRow("ws-drift", []byte(`{"required":">= 1.5.0","actual":"1.4.0","satisfies":false,"status":"drift"}`)).
			AddRow("ws-unknown", []byte(`{"required":">= 1.5.0","actual":"","satisfies":false,"status":"unknown"}`)),
		)

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
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetVersionDrift_NoCompletedRun(t *testing.T) {
	r, mock := newTestRouter(t)

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
	r, mock := newTestRouter(t)
	runID := "33333333-3333-3333-3333-333333333333"

	expectLatestCompletedRun(mock, runID)

	mock.ExpectQuery(`SELECT workspace_name, version_drift_report\s+FROM analysis_results`).
		WithArgs(runID, "success").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_name", "version_drift_report"}))

	w := doGet(t, r, "/dashboard/version-drift")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Data VersionDriftResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Data.Total)
	require.Empty(t, resp.Data.Entries)
	require.NoError(t, mock.ExpectationsWereMet())
}
