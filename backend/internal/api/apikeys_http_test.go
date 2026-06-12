package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// apiKeysEnv wires APIKeysHandlers behind a context stub standing in for the
// auth middleware (user u1 with mutable scopes).
type apiKeysEnv struct {
	*sourcesEnv
	scopes *[]string
}

func newAPIKeysEnv(t *testing.T) *apiKeysEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	scopes := []string{"state:read", "state:drift"}
	h := NewAPIKeysHandlers(db)
	h.audit = newAuditor(nil) // keep audit writes off the sqlmock rig
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", "u1")
		c.Set("scopes", scopes)
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/apikeys", h.ListAPIKeys())
	v1.POST("/apikeys", h.CreateAPIKey())
	v1.GET("/apikeys/:id", h.GetAPIKey())
	v1.PUT("/apikeys/:id", h.UpdateAPIKey())
	v1.DELETE("/apikeys/:id", h.DeleteAPIKey())
	v1.POST("/apikeys/:id/rotate", h.RotateAPIKey())
	return &apiKeysEnv{sourcesEnv: &sourcesEnv{r: r, mock: mock}, scopes: &scopes}
}

var apiKeyRowCols = []string{"id", "user_id", "organization_id", "name", "description", "key_hash",
	"key_prefix", "scopes", "expires_at", "last_used_at", "expiry_notification_sent_at", "created_at"}

func apiKeyDBRow(id, userID, scopes string) *sqlmock.Rows {
	return sqlmock.NewRows(apiKeyRowCols).
		AddRow(id, userID, "org1", "ci-key", nil, "$2a$12$hashhashhash", "tsm_abc123",
			[]byte(scopes), nil, nil, nil, time.Now())
}

func expectDefaultOrg(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("FROM organizations").WithArgs("default").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}).
			AddRow("org1", "default", "Default", nil, nil, time.Now(), time.Now()))
}

func TestAPIKeys_CreateReturnsSecretOnce(t *testing.T) {
	e := newAPIKeysEnv(t)
	expectDefaultOrg(e.mock)
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"ci-key","scopes":["state:read","state:drift"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"key":"tsm_`) {
		t.Errorf("plaintext secret must be returned exactly once on create: %s", body)
	}
	if strings.Contains(body, `"key_hash"`) || strings.Contains(body, "$2a$") {
		t.Errorf("bcrypt hash must never serialize: %s", body)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestAPIKeys_CreateValidation(t *testing.T) {
	e := newAPIKeysEnv(t)

	if w := e.do(http.MethodPost, "/api/v1/apikeys", `{"scopes":["state:read"]}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing name: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"k"}`); w.Code != http.StatusBadRequest {
		t.Errorf("no scopes: %d", w.Code)
	}

	// Scope-grant rule: keys may only carry scopes the creator holds.
	w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"k","scopes":["sources:manage"]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "sources:manage") {
		t.Errorf("ungranted scope: %d (%s)", w.Code, w.Body.String())
	}
	// Unknown scopes are rejected even for admins.
	*e.scopes = []string{"admin"}
	if w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"k","scopes":["scim:provision"]}`); w.Code != http.StatusBadRequest {
		t.Errorf("non-assignable scope: %d", w.Code)
	}
	// write implies read for granting (rw pair).
	*e.scopes = []string{"state:write"}
	expectDefaultOrg(e.mock)
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"k","scopes":["state:read"]}`); w.Code != http.StatusCreated {
		t.Errorf("write-implies-read grant: %d (%s)", w.Code, w.Body.String())
	}

	// Past expiry rejected.
	if w := e.do(http.MethodPost, "/api/v1/apikeys",
		`{"name":"k","scopes":["state:read"],"expires_at":"2020-01-01T00:00:00Z"}`); w.Code != http.StatusBadRequest {
		t.Errorf("past expiry: %d", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/apikeys",
		`{"name":"k","scopes":["state:read"],"expires_at":"not-a-date"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad expiry: %d", w.Code)
	}
}

func TestAPIKeys_ListOwnVsAdmin(t *testing.T) {
	e := newAPIKeysEnv(t)

	// Non-admin: own keys only (WHERE ak.user_id = $1).
	e.mock.ExpectQuery("FROM api_keys ak").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(append(apiKeyRowCols, "user_name")).
			AddRow("k1", "u1", "org1", "mine", nil, "h", "tsm_abc123", []byte(`["state:read"]`), nil, nil, nil, time.Now(), "Me"))
	w := e.do(http.MethodGet, "/api/v1/apikeys", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mine"`) {
		t.Fatalf("own list: %d (%s)", w.Code, w.Body.String())
	}

	// Admin: every key.
	*e.scopes = []string{"admin"}
	e.mock.ExpectQuery("FROM api_keys ak").
		WillReturnRows(sqlmock.NewRows(append(apiKeyRowCols, "user_name")).
			AddRow("k2", "u2", "org1", "theirs", nil, "h", "tsm_def456", []byte(`["state:read"]`), nil, nil, nil, time.Now(), "Them"))
	w = e.do(http.MethodGet, "/api/v1/apikeys", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"theirs"`) {
		t.Fatalf("admin list: %d (%s)", w.Code, w.Body.String())
	}
}

func TestAPIKeys_OwnershipBoundary(t *testing.T) {
	e := newAPIKeysEnv(t)

	// Another user's key reads as 404 for non-admins (existence hidden).
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k2").
		WillReturnRows(apiKeyDBRow("k2", "u2", `["state:read"]`))
	if w := e.do(http.MethodGet, "/api/v1/apikeys/k2", ""); w.Code != http.StatusNotFound {
		t.Errorf("foreign key get: %d", w.Code)
	}
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k2").
		WillReturnRows(apiKeyDBRow("k2", "u2", `["state:read"]`))
	if w := e.do(http.MethodDelete, "/api/v1/apikeys/k2", ""); w.Code != http.StatusNotFound {
		t.Errorf("foreign key delete: %d", w.Code)
	}

	// Admin can manage it.
	*e.scopes = []string{"admin"}
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k2").
		WillReturnRows(apiKeyDBRow("k2", "u2", `["state:read"]`))
	e.mock.ExpectExec("DELETE FROM api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/apikeys/k2", ""); w.Code != http.StatusNoContent {
		t.Errorf("admin delete: %d", w.Code)
	}
}

func TestAPIKeys_UpdateRevalidatesGrants(t *testing.T) {
	e := newAPIKeysEnv(t)

	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	w := e.do(http.MethodPut, "/api/v1/apikeys/k1", `{"name":"ci-key","scopes":["admin"]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("escalation via update must be rejected: %d (%s)", w.Code, w.Body.String())
	}

	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	e.mock.ExpectExec("UPDATE api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPut, "/api/v1/apikeys/k1", `{"name":"renamed","scopes":["state:read"]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"renamed"`) {
		t.Errorf("update: %d (%s)", w.Code, w.Body.String())
	}
}

func TestAPIKeys_Rotate(t *testing.T) {
	e := newAPIKeysEnv(t)

	// Immediate rotation: mint replacement, revoke old.
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	expectDefaultOrg(e.mock)
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	w := e.do(http.MethodPost, "/api/v1/apikeys/k1/rotate", `{"grace_period_hours":0}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"key":"tsm_`) {
		t.Fatalf("immediate rotate: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("immediate rotation must revoke the old key: %v", err)
	}

	// Grace rotation: old key gets an expiry instead of deletion. (The default
	// org is cached per repository, so no second org query.)
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read"]`))
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPost, "/api/v1/apikeys/k1/rotate", `{"grace_period_hours":24}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("grace rotate: %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("grace rotation must schedule old-key expiry: %v", err)
	}

	if w := e.do(http.MethodPost, "/api/v1/apikeys/k1/rotate", `{"grace_period_hours":100}`); w.Code != http.StatusBadRequest {
		t.Errorf("grace > 72h: %d", w.Code)
	}
}
