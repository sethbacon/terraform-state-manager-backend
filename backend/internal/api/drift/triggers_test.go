package drift

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	captrigger "github.com/terraform-state-manager/terraform-state-manager/internal/capability/drifttrigger"
	capenvdrift "github.com/terraform-state-manager/terraform-state-manager/internal/capability/envdrift"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/cloud/azure"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	triggersvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/drifttrigger"
	envdriftsvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/envdrift"
)

// fakeTriggerEngine is a captrigger.Engine that records the request and returns a
// canned result, so the Trigger endpoint can be exercised without live ADO/WIF.
type fakeTriggerEngine struct {
	result *triggersvc.TriggerResult
	err    error
}

func (f *fakeTriggerEngine) Trigger(_ context.Context, _ triggersvc.TriggerRequest) (*triggersvc.TriggerResult, error) {
	return f.result, f.err
}

// newTriggerRouter wires a router for the manual drift triggers with the given
// capability handlers and an org id injected into the request context.
func newTriggerRouter(
	t *testing.T,
	orgID string,
	envDrift *capenvdrift.Handler,
	trigger *captrigger.Handler,
) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driftRepo := repositories.NewDriftEventRepository(db)
	sourceRepo := repositories.NewStateSourceRepository(db)
	h := NewHandlers(nil, driftRepo, sourceRepo).WithCapabilities(envDrift, trigger)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		if orgID != "" {
			c.Set("organization_id", orgID)
		}
		c.Next()
	})
	r.POST("/drift/env-check", h.EnvCheck)
	r.POST("/drift/trigger", h.Trigger)
	return r, mock
}

func expectSource(mock sqlmock.Sqlmock, sourceID, orgID, name string) {
	cols := []string{
		"id", "organization_id", "name", "source_type", "config", "is_active",
		"last_tested_at", "last_test_status", "created_by", "created_at", "updated_at",
	}
	rows := sqlmock.NewRows(cols).AddRow(
		sourceID, orgID, name, "s3", []byte(`{}`), true,
		nil, nil, nil, time.Now(), time.Now(),
	)
	mock.ExpectQuery(`SELECT id, organization_id, name, source_type, config`).
		WithArgs(sourceID).WillReturnRows(rows)
}

func postJSON(t *testing.T, r *gin.Engine, path string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const triggerTestOrg = "11111111-1111-1111-1111-111111111111"

// --- env-check -----------------------------------------------------------------

func TestEnvCheck_Unconfigured_Returns503(t *testing.T) {
	// envDrift handler with nil engine => unconfigured.
	r, _ := newTriggerRouter(t, triggerTestOrg, capenvdrift.NewHandler(nil), nil)
	w := postJSON(t, r, "/drift/env-check", map[string]interface{}{
		"source_id": uuid.NewString(),
		"state":     map[string]interface{}{"version": 4},
	})
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}

func TestEnvCheck_NilCapability_Returns503(t *testing.T) {
	r, _ := newTriggerRouter(t, triggerTestOrg, nil, nil)
	w := postJSON(t, r, "/drift/env-check", map[string]interface{}{
		"source_id": uuid.NewString(),
		"state":     map[string]interface{}{"version": 4},
	})
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}

func TestEnvCheck_NoDrift_Returns200(t *testing.T) {
	// A stub reader where every recorded resource is present and matching =>
	// existence-only comparison yields no drift. An empty state has no azurerm
	// resources, so no drift event is written and no DB insert occurs.
	reader := azure.NewStubReader(map[string]azure.ResourceState{})
	driftRepo := repositories.NewDriftEventRepository(nil) // unused for empty state
	engine := envdriftsvc.NewService(reader, driftRepo)
	envHandler := capenvdrift.NewHandler(engine)

	sourceID := uuid.NewString()
	r, mock := newTriggerRouter(t, triggerTestOrg, envHandler, nil)
	expectSource(mock, sourceID, triggerTestOrg, "prod-ws")

	w := postJSON(t, r, "/drift/env-check", map[string]interface{}{
		"source_id": sourceID,
		"state":     &hcp.StateFile{Version: 4, Resources: []hcp.StateResource{}},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Workspace     string `json:"workspace"`
		DriftDetected bool   `json:"drift_detected"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "prod-ws", resp.Workspace)
	require.False(t, resp.DriftDetected)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEnvCheck_CrossOrgSource_Returns403(t *testing.T) {
	engine := envdriftsvc.NewService(azure.NewStubReader(map[string]azure.ResourceState{}), repositories.NewDriftEventRepository(nil))
	envHandler := capenvdrift.NewHandler(engine)

	sourceID := uuid.NewString()
	r, mock := newTriggerRouter(t, triggerTestOrg, envHandler, nil)
	// Source belongs to a DIFFERENT org.
	expectSource(mock, sourceID, "22222222-2222-2222-2222-222222222222", "other-ws")

	w := postJSON(t, r, "/drift/env-check", map[string]interface{}{
		"source_id": sourceID,
		"state":     &hcp.StateFile{Version: 4},
	})
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

func TestEnvCheck_MissingOrg_Returns400(t *testing.T) {
	engine := envdriftsvc.NewService(azure.NewStubReader(map[string]azure.ResourceState{}), repositories.NewDriftEventRepository(nil))
	r, _ := newTriggerRouter(t, "", capenvdrift.NewHandler(engine), nil)
	w := postJSON(t, r, "/drift/env-check", map[string]interface{}{
		"source_id": uuid.NewString(),
		"state":     &hcp.StateFile{Version: 4},
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// --- trigger -------------------------------------------------------------------

func TestTrigger_Unconfigured_Returns503(t *testing.T) {
	r, _ := newTriggerRouter(t, triggerTestOrg, nil, captrigger.NewHandler(nil))
	w := postJSON(t, r, "/drift/trigger", map[string]interface{}{
		"source_id":   uuid.NewString(),
		"pipeline_id": 7,
	})
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}

func TestTrigger_NilCapability_Returns503(t *testing.T) {
	r, _ := newTriggerRouter(t, triggerTestOrg, nil, nil)
	w := postJSON(t, r, "/drift/trigger", map[string]interface{}{
		"source_id":   uuid.NewString(),
		"pipeline_id": 7,
	})
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}

func TestTrigger_QueuesRun_Returns202(t *testing.T) {
	engine := &fakeTriggerEngine{result: &triggersvc.TriggerResult{RunID: 99, RunState: "inProgress", RunURL: "https://ado/run/99"}}
	trigger := captrigger.NewHandler(engine)

	sourceID := uuid.NewString()
	r, mock := newTriggerRouter(t, triggerTestOrg, nil, trigger)
	expectSource(mock, sourceID, triggerTestOrg, "prod-ws")

	w := postJSON(t, r, "/drift/trigger", map[string]interface{}{
		"source_id":   sourceID,
		"pipeline_id": 7,
	})
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	var resp struct {
		RunID    int    `json:"run_id"`
		RunState string `json:"run_state"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 99, resp.RunID)
	require.Equal(t, "inProgress", resp.RunState)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTrigger_MissingPipelineID_Returns400(t *testing.T) {
	trigger := captrigger.NewHandler(&fakeTriggerEngine{result: &triggersvc.TriggerResult{}})
	r, _ := newTriggerRouter(t, triggerTestOrg, nil, trigger)
	w := postJSON(t, r, "/drift/trigger", map[string]interface{}{
		"source_id": uuid.NewString(),
		// pipeline_id omitted => binding:"required" rejects with 400.
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestTrigger_CrossOrgSource_Returns403(t *testing.T) {
	trigger := captrigger.NewHandler(&fakeTriggerEngine{result: &triggersvc.TriggerResult{RunID: 1}})
	sourceID := uuid.NewString()
	r, mock := newTriggerRouter(t, triggerTestOrg, nil, trigger)
	expectSource(mock, sourceID, "22222222-2222-2222-2222-222222222222", "other-ws")

	w := postJSON(t, r, "/drift/trigger", map[string]interface{}{
		"source_id":   sourceID,
		"pipeline_id": 7,
	})
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
