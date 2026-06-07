package drift

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/driftingest"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

const (
	testIssuer   = "https://login.example.test/tenant"
	testAudience = "api://tsm-drift-ingest"
	testKeyID    = "k1"
	testOrgID    = "11111111-1111-1111-1111-111111111111"
)

// --- mock JWKS / token signing -------------------------------------------------

type jwksFixture struct {
	priv   *rsa.PrivateKey
	server *httptest.Server
}

func newJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       priv.Public(),
		KeyID:     testKeyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}}

	mux := http.NewServeMux()
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &jwksFixture{priv: priv, server: srv}
}

func (j *jwksFixture) signToken(t *testing.T) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: j.priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", testKeyID),
	)
	require.NoError(t, err)
	now := time.Now()
	raw, err := jwt.Signed(signer).Claims(map[string]interface{}{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "pipeline-sp",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}).Serialize()
	require.NoError(t, err)
	return raw
}

func (j *jwksFixture) validator() *driftingest.Validator {
	keySet := oidc.NewRemoteKeySet(context.Background(), j.server.URL+"/jwks.json")
	verifier := oidc.NewVerifier(testIssuer, keySet, &oidc.Config{ClientID: testAudience})
	return driftingest.NewValidatorWithVerifierForTest(verifier, testIssuer, testAudience)
}

// --- test harness --------------------------------------------------------------

func newTestRouter(t *testing.T, validator *driftingest.Validator) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driftRepo := repositories.NewDriftEventRepository(db)
	sourceRepo := repositories.NewStateSourceRepository(db)
	h := NewHandlers(validator, driftRepo, sourceRepo)

	r := gin.New()
	r.POST("/drift/ingest", h.IngestPlan)
	return r, mock
}

func expectSourceLookup(mock sqlmock.Sqlmock, sourceID, orgID, name string) {
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

func doPost(t *testing.T, r *gin.Engine, token string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/drift/ingest", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func loadFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "services", "driftingest", "testdata", name))
	require.NoError(t, err)
	return json.RawMessage(data)
}

// --- tests ---------------------------------------------------------------------

func TestIngest_ValidTokenWithChanges_CreatesCodeDriftEvent(t *testing.T) {
	js := newJWKSFixture(t)
	r, mock := newTestRouter(t, js.validator())

	sourceID := uuid.NewString()
	externalRef := "ado-run-42"

	expectSourceLookup(mock, sourceID, testOrgID, "prod-workspace")

	// Idempotency check: no existing event for this external_ref.
	mock.ExpectQuery(`SELECT id, organization_id, workspace_name.*FROM drift_events\s+WHERE organization_id = \$1 AND external_ref = \$2`).
		WithArgs(testOrgID, externalRef).
		WillReturnRows(sqlmock.NewRows([]string{}))

	// Insert the new code-sourced event; capture drift_source + external_ref args.
	newID := uuid.NewString()
	mock.ExpectQuery(`INSERT INTO drift_events`).
		WithArgs(
			testOrgID, "prod-workspace", nil, nil, sqlmock.AnyArg(),
			"critical", "code", externalRef,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "detected_at"}).AddRow(newID, time.Now()))

	w := doPost(t, r, js.signToken(t), map[string]interface{}{
		"source_id":       sourceID,
		"plan":            loadFixture(t, "plan_with_changes.json"),
		"changes_present": true,
		"external_ref":    externalRef,
	})

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var resp struct {
		Data struct {
			ID          string `json:"id"`
			DriftSource string `json:"drift_source"`
			Severity    string `json:"severity"`
			ExternalRef string `json:"external_ref"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "code", resp.Data.DriftSource)
	require.Equal(t, "critical", resp.Data.Severity) // a delete present => critical
	require.Equal(t, externalRef, resp.Data.ExternalRef)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIngest_DuplicateExternalRef_IsIdempotent(t *testing.T) {
	js := newJWKSFixture(t)
	r, mock := newTestRouter(t, js.validator())

	sourceID := uuid.NewString()
	externalRef := "ado-run-42"
	existingID := uuid.NewString()

	expectSourceLookup(mock, sourceID, testOrgID, "prod-workspace")

	driftCols := []string{
		"id", "organization_id", "workspace_name", "snapshot_before",
		"snapshot_after", "changes", "severity", "drift_source", "external_ref", "detected_at",
	}
	mock.ExpectQuery(`SELECT id, organization_id, workspace_name.*FROM drift_events\s+WHERE organization_id = \$1 AND external_ref = \$2`).
		WithArgs(testOrgID, externalRef).
		WillReturnRows(sqlmock.NewRows(driftCols).AddRow(
			existingID, testOrgID, "prod-workspace", nil, nil,
			[]byte(`{"added":["x"]}`), "warning", "code", externalRef, time.Now(),
		))

	// No INSERT expected — duplicate short-circuits.
	w := doPost(t, r, js.signToken(t), map[string]interface{}{
		"source_id":       sourceID,
		"plan":            loadFixture(t, "plan_with_changes.json"),
		"changes_present": true,
		"external_ref":    externalRef,
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Idempotent bool `json:"idempotent"`
		Data       struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Idempotent)
	require.Equal(t, existingID, resp.Data.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIngest_NoOpPlanWithoutChangesFlag_WritesNoEvent(t *testing.T) {
	js := newJWKSFixture(t)
	r, mock := newTestRouter(t, js.validator())

	sourceID := uuid.NewString()
	externalRef := "ado-run-noop"

	expectSourceLookup(mock, sourceID, testOrgID, "prod-workspace")
	mock.ExpectQuery(`SELECT id, organization_id, workspace_name.*FROM drift_events\s+WHERE organization_id = \$1 AND external_ref = \$2`).
		WithArgs(testOrgID, externalRef).
		WillReturnRows(sqlmock.NewRows([]string{}))

	// No INSERT expected — a no-op plan with changes_present=false records nothing.
	w := doPost(t, r, js.signToken(t), map[string]interface{}{
		"source_id":       sourceID,
		"plan":            loadFixture(t, "plan_no_changes.json"),
		"changes_present": false,
		"external_ref":    externalRef,
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		ChangesPresent bool `json:"changes_present"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.False(t, resp.ChangesPresent)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIngest_MissingToken_Returns401(t *testing.T) {
	js := newJWKSFixture(t)
	r, _ := newTestRouter(t, js.validator())

	w := doPost(t, r, "", map[string]interface{}{
		"source_id":    uuid.NewString(),
		"external_ref": "x",
	})
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

func TestIngest_InvalidToken_Returns401(t *testing.T) {
	js := newJWKSFixture(t)
	r, _ := newTestRouter(t, js.validator())

	// A token signed by a different key than the served JWKS is invalid.
	other := newJWKSFixture(t)
	w := doPost(t, r, other.signToken(t), map[string]interface{}{
		"source_id":    uuid.NewString(),
		"external_ref": "x",
	})
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

func TestIngest_NoValidatorConfigured_Returns503(t *testing.T) {
	r, _ := newTestRouter(t, nil)
	js := newJWKSFixture(t)
	w := doPost(t, r, js.signToken(t), map[string]interface{}{
		"source_id":    uuid.NewString(),
		"external_ref": "x",
	})
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}
