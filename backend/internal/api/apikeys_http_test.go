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

// TestAPIKeys_AdminScopeNotAssignable proves an admin cannot mint an
// admin-scoped API key: ScopeAdmin is excluded from assignableKeyScopes, so
// admin stays bound to the interactive session rather than a durable, CSRF- and
// TTL-exempt bearer key (#252). Rejected before any INSERT (no DB expectation).
func TestAPIKeys_AdminScopeNotAssignable(t *testing.T) {
	e := newAPIKeysEnv(t)
	*e.scopes = []string{"admin"}
	w := e.do(http.MethodPost, "/api/v1/apikeys", `{"name":"k","scopes":["admin"]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "admin") {
		t.Errorf("admin scope must not be key-assignable even for an admin creator: %d (%s)", w.Code, w.Body.String())
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

	// Admin: keys are narrowed to those whose OWNER shares an org the caller
	// administers (#182). Keys are all tagged with the default org at mint time,
	// so the owner's membership — not the key's org — is the tenant boundary. u1 is
	// admin in org-a; key-a's owner (u-a) is in org-a (kept), key-b's owner (u-b)
	// is only in org-b (excluded).
	*e.scopes = []string{"admin"}
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("org-a", "A", "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`)))
	e.mock.ExpectQuery("FROM api_keys ak").
		WillReturnRows(sqlmock.NewRows(append(apiKeyRowCols, "user_name")).
			AddRow("k-a", "u-a", "default", "key-a", nil, "h", "tsm_aaa111", []byte(`["state:read"]`), nil, nil, nil, time.Now(), "UserA").
			AddRow("k-b", "u-b", "default", "key-b", nil, "h", "tsm_bbb222", []byte(`["state:read"]`), nil, nil, nil, time.Now(), "UserB"))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u-a").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("org-a", "A", "rt-x", time.Now(), "editor", "Editor", []byte(`["state:read"]`)))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u-b").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("org-b", "B", "rt-x", time.Now(), "editor", "Editor", []byte(`["state:read"]`)))
	w = e.do(http.MethodGet, "/api/v1/apikeys", "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin list: %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"key-a"`) {
		t.Errorf("admin should see key-a (owner shares admin org-a): %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"key-b"`) {
		t.Errorf("admin must NOT see key-b (owner only in non-admin org-b): %s", w.Body.String())
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

// apiKeyAdminListCols mirrors ListAPIKeys' projection, which joins users for the
// owner's display name and so carries one column more than apiKeyRowCols.
var apiKeyAdminListCols = append(append([]string{}, apiKeyRowCols...), "user_name")

// TestAPIKeys_AdminListNarrowsToOwnersSharingAnAdminOrg is the #182 guard for
// the admin key view, and it exists because identity v0.25.0 makes it easy to
// delete by accident.
//
// v0.25.0 replaced ListAll with ListAPIKeys(ctx, scope) and says, correctly for
// its other consumer, that the in-memory admin-organization filter beside it is
// now the query's own predicate. That is NOT true here: ListAPIKeys scopes on
// ak.organization_id, and apikeys.mintKey stamps EVERY TSM key with the GLOBAL
// DEFAULT organization whoever owns it. Swapping the owner-membership filter for
// an org-scoped query would therefore show an admin of the default organization
// every other tenant's keys — including their bcrypt key_hash — and an admin of
// any other organization none at all. The tenant boundary here is the OWNER's
// membership, and this test pins it.
func TestAPIKeys_AdminListNarrowsToOwnersSharingAnAdminOrg(t *testing.T) {
	e := newAPIKeysEnv(t)
	*e.scopes = []string{"admin"}

	// The caller administers org-a only.
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(userMembershipCols).
			AddRow("org-a", "Org A", "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`)).
			AddRow("org-c", "Org C", "rt-viewer", time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	// Every key carries the default organization, which is why the row's own
	// organization_id cannot be the filter.
	e.mock.ExpectQuery("FROM api_keys").WillReturnRows(
		sqlmock.NewRows(apiKeyAdminListCols).
			AddRow("k-shared", "owner-shared", "org1", "ci", nil, "h", "tsm_a", []byte(`["state:read"]`), nil, nil, nil, time.Now(), "Shared").
			AddRow("k-other", "owner-other", "org1", "ci", nil, "h", "tsm_b", []byte(`["state:read"]`), nil, nil, nil, time.Now(), "Other").
			AddRow("k-orphan", "owner-orphan", "org1", "ci", nil, "h", "tsm_c", []byte(`["state:read"]`), nil, nil, nil, time.Now(), "Orphan"))
	// One membership lookup per distinct owner, in row order.
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("owner-shared").
		WillReturnRows(sqlmock.NewRows(userMembershipCols).
			AddRow("org-a", "Org A", "rt-viewer", time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("owner-other").
		WillReturnRows(sqlmock.NewRows(userMembershipCols).
			AddRow("org-b", "Org B", "rt-viewer", time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("owner-orphan").
		WillReturnRows(sqlmock.NewRows(userMembershipCols))

	w := e.do(http.MethodGet, "/api/v1/apikeys", "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin list: status = %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "k-shared") {
		t.Errorf("a key whose owner shares the caller's admin organization must be listed: %s", body)
	}
	if strings.Contains(body, "k-other") {
		t.Errorf("a key whose owner belongs only to ANOTHER tenant must not be listed (#182): %s", body)
	}
	if !strings.Contains(body, "k-orphan") {
		t.Errorf("a key whose owner belongs to no organization has no cross-tenant boundary "+
			"to protect and must stay listed, mirroring the user list: %s", body)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("queued round-trips did not all run: %v", err)
	}
}
