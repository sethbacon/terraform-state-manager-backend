package compliance

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	compliancesvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/compliance"
)

const testOrgID = "11111111-1111-1111-1111-111111111111"

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestHandlers wires the compliance handlers over a mock database. The
// returned Checker uses the real engine registry, so engine_type validation and
// the engines listing reflect the genuinely registered engines (custom + opa).
func newTestHandlers(t *testing.T) (*Handlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	policyRepo := repositories.NewCompliancePolicyRepository(db)
	resultRepo := repositories.NewComplianceResultRepository(db)
	checker := compliancesvc.NewChecker(policyRepo, resultRepo)
	return NewHandlers(policyRepo, resultRepo, checker), mock
}

// newOrgContext builds a gin context carrying the JSON body and an
// organization_id, mimicking what the auth middleware injects upstream.
func newOrgContext(method, target string, body interface{}) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reader *bytes.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	c.Request = httptest.NewRequest(method, target, reader)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("organization_id", testOrgID)
	return c, w
}

// policyColumns mirrors the SELECT column order used by the repository so mocked
// rows scan correctly.
var policyColumns = []string{
	"id", "organization_id", "name", "policy_type", "config",
	"severity", "engine_type", "is_active", "created_at", "updated_at",
}

func policyRow(id, engineType string) *sqlmock.Rows {
	return sqlmock.NewRows(policyColumns).AddRow(
		id, testOrgID, "test-policy", "tagging", []byte(`{"required_tags":["env"]}`),
		"warning", engineType, true, time.Now(), time.Now(),
	)
}

// --- CreatePolicy ---------------------------------------------------------------

func TestCreatePolicy_RoundTripsEngineType(t *testing.T) {
	h, mock := newTestHandlers(t)

	newID := uuid.NewString()
	// The INSERT must carry engine_type "opa" as the 6th positional arg.
	mock.ExpectQuery(`INSERT INTO compliance_policies`).
		WithArgs(
			testOrgID, "test-policy", "tagging", sqlmock.AnyArg(),
			"warning", "opa", true,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
			AddRow(newID, time.Now(), time.Now()))

	c, w := newOrgContext(http.MethodPost, "/compliance/policies", map[string]interface{}{
		"name":        "test-policy",
		"policy_type": "tagging",
		"config":      json.RawMessage(`{"required_tags":["env"]}`),
		"engine_type": "opa",
	})
	h.CreatePolicy(c)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp struct {
		Data struct {
			EngineType string `json:"engine_type"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "opa", resp.Data.EngineType)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreatePolicy_DefaultsEngineTypeToCustom(t *testing.T) {
	h, mock := newTestHandlers(t)

	newID := uuid.NewString()
	// Omitting engine_type must persist the default "custom".
	mock.ExpectQuery(`INSERT INTO compliance_policies`).
		WithArgs(
			testOrgID, "test-policy", "tagging", sqlmock.AnyArg(),
			"warning", "custom", true,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
			AddRow(newID, time.Now(), time.Now()))

	c, w := newOrgContext(http.MethodPost, "/compliance/policies", map[string]interface{}{
		"name":        "test-policy",
		"policy_type": "tagging",
		"config":      json.RawMessage(`{"required_tags":["env"]}`),
	})
	h.CreatePolicy(c)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp struct {
		Data struct {
			EngineType string `json:"engine_type"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "custom", resp.Data.EngineType)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreatePolicy_RejectsUnknownEngineType(t *testing.T) {
	h, mock := newTestHandlers(t)

	// No DB interaction expected — validation rejects before any insert.
	c, w := newOrgContext(http.MethodPost, "/compliance/policies", map[string]interface{}{
		"name":        "test-policy",
		"policy_type": "tagging",
		"config":      json.RawMessage(`{"required_tags":["env"]}`),
		"engine_type": "bogus",
	})
	h.CreatePolicy(c)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "engine_type")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- UpdatePolicy ---------------------------------------------------------------

func TestUpdatePolicy_RoundTripsEngineType(t *testing.T) {
	h, mock := newTestHandlers(t)

	id := uuid.NewString()
	// Load existing policy (currently "custom").
	mock.ExpectQuery(`SELECT id, organization_id, name, policy_type, config, severity, engine_type`).
		WithArgs(id).
		WillReturnRows(policyRow(id, "custom"))
	// Update must write engine_type "opa" (5th positional arg in the UPDATE).
	mock.ExpectExec(`UPDATE compliance_policies`).
		WithArgs(
			"test-policy", "tagging", sqlmock.AnyArg(), "warning",
			"opa", true, sqlmock.AnyArg(), id,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	c, w := newOrgContext(http.MethodPut, "/compliance/policies/"+id, map[string]interface{}{
		"engine_type": "opa",
	})
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.UpdatePolicy(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Data struct {
			EngineType string `json:"engine_type"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "opa", resp.Data.EngineType)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdatePolicy_RejectsUnknownEngineType(t *testing.T) {
	h, mock := newTestHandlers(t)

	id := uuid.NewString()
	// Validation rejects before the policy is loaded — no DB interaction expected.
	c, w := newOrgContext(http.MethodPut, "/compliance/policies/"+id, map[string]interface{}{
		"engine_type": "bogus",
	})
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.UpdatePolicy(c)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "engine_type")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- ListEngines ----------------------------------------------------------------

func TestListEngines_ReturnsRegisteredEngines(t *testing.T) {
	h, _ := newTestHandlers(t)

	c, w := newOrgContext(http.MethodGet, "/compliance/engines", nil)
	h.ListEngines(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	names := make([]string, 0, len(resp.Data))
	for _, e := range resp.Data {
		names = append(names, e.Name)
	}
	require.Equal(t, resp.Total, len(resp.Data))
	require.Contains(t, names, "custom")
	require.Contains(t, names, "opa")
}
