package scim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func init() { gin.SetMode(gin.TestMode) }

var userCols = []string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}

func userRow(id, email, name string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(userCols).AddRow(id, email, name, "scim:ext-1", now, now)
}

// newSCIM wires the handler set over a sqlmock identity DB and the real route
// shapes; bearer auth/scope gating live in the router and are tested there.
func newSCIM(t *testing.T) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	cfg.Server.PublicURL = "https://tsm.example.com"
	h := NewHandlers(cfg, db, nil)

	r := gin.New()
	v2 := r.Group("/scim/v2")
	v2.GET("/Users", h.ListUsers())
	v2.POST("/Users", h.CreateUser())
	v2.GET("/Users/:id", h.GetUser())
	v2.PATCH("/Users/:id", h.PatchUser())
	v2.PUT("/Users/:id", h.PutUser())
	v2.DELETE("/Users/:id", h.DeleteUser())
	v2.GET("/Groups", h.ListGroups())
	v2.GET("/Groups/:id", h.GetGroup())
	return r, mock
}

func doJSON(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// removedOrgRows is what RemoveAllMembershipsForUser returns since identity
// v0.25.0: the organizations whose membership it actually removed, as
// RETURNING rows rather than an affected-row count. n rows stand in for the
// count the old expectations asserted.
func removedOrgRows(n int) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"organization_id"})
	for i := 0; i < n; i++ {
		rows.AddRow(fmt.Sprintf("o%d", i+1))
	}
	return rows
}

func TestListUsers(t *testing.T) {
	r, mock := newSCIM(t)

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs(100, 0).
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))

	w := doJSON(r, http.MethodGet, "/scim/v2/Users", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var resp SCIMListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.TotalResults != 1 || resp.StartIndex != 1 || resp.Schemas[0] != SchemaListResp {
		t.Errorf("list envelope wrong: %+v", resp)
	}
}

func TestListUsers_FilterUsesSearch(t *testing.T) {
	r, mock := newSCIM(t)

	// The scope predicate is spliced in before the LIMIT/OFFSET, so the WHERE
	// clause no longer ends at the ILIKE pair.
	mock.ExpectQuery(`WHERE \(email ILIKE`).WithArgs("%alice%", 100, 0).
		WillReturnRows(userRow("u1", "alice@b.c", "Alice"))

	w := doJSON(r, http.MethodGet, `/scim/v2/Users?filter=userName+eq+"alice"`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestListUsers_DBError(t *testing.T) {
	r, _ := newSCIM(t)
	w := doJSON(r, http.MethodGet, "/scim/v2/Users", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), SchemaError) {
		t.Errorf("error not in SCIM error shape: %s", w.Body.String())
	}
}

func TestGetUser(t *testing.T) {
	r, mock := newSCIM(t)

	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))
	w := doJSON(r, http.MethodGet, "/scim/v2/Users/u1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var u SCIMUser
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.UserName != "a@b.c" || u.Active == nil || !*u.Active || u.Schemas[0] != SchemaUser {
		t.Errorf("SCIM user shape wrong: %+v", u)
	}
	if !strings.HasPrefix(u.Meta.Location, "https://tsm.example.com/") {
		t.Errorf("meta.location should use the configured public URL: %q", u.Meta.Location)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	r, mock := newSCIM(t)
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(userCols))
	w := doJSON(r, http.MethodGet, "/scim/v2/Users/ghost", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestCreateUser(t *testing.T) {
	r, mock := newSCIM(t)

	// Existing user with the same external id → resolved, no insert.
	mock.ExpectQuery("WHERE oidc_sub").WithArgs("scim:ext-1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))

	body := `{"schemas":["` + SchemaUser + `"],"externalId":"ext-1","userName":"a@b.c","name":{"formatted":"Alice"},"active":true}`
	w := doJSON(r, http.MethodPost, "/scim/v2/Users", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestCreateUser_Validation(t *testing.T) {
	r, _ := newSCIM(t)

	if w := doJSON(r, http.MethodPost, "/scim/v2/Users", "{not json"); w.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: status = %d, want 400", w.Code)
	}
	if w := doJSON(r, http.MethodPost, "/scim/v2/Users", `{"active":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing userName/emails: status = %d, want 400", w.Code)
	}
}

func TestPatchUser_DeactivateRemovesMemberships(t *testing.T) {
	r, mock := newSCIM(t)

	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))
	mock.ExpectQuery("DELETE FROM organization_members").WithArgs("u1").
		WillReturnRows(removedOrgRows(2))
	mock.ExpectExec("UPDATE users").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"schemas":["` + SchemaPatchOp + `"],"Operations":[{"op":"replace","path":"active","value":false}]}`
	w := doJSON(r, http.MethodPatch, "/scim/v2/Users/u1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("deactivation must remove memberships: %v", err)
	}
}

func TestPatchUser_RenameAndNoPathMap(t *testing.T) {
	r, mock := newSCIM(t)

	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))
	mock.ExpectExec("UPDATE users").
		WithArgs("u1", "new@b.c", "New Name", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"Operations":[
		{"op":"replace","path":"userName","value":"new@b.c"},
		{"op":"replace","path":"","value":{"name":{"formatted":"New Name"}}}
	]}`
	w := doJSON(r, http.MethodPatch, "/scim/v2/Users/u1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("rename should update email+name: %v", err)
	}
}

func TestPatchUser_NotFoundAndBadJSON(t *testing.T) {
	r, mock := newSCIM(t)

	if w := doJSON(r, http.MethodPatch, "/scim/v2/Users/u1", "{nope"); w.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: status = %d, want 400", w.Code)
	}

	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(userCols))
	w := doJSON(r, http.MethodPatch, "/scim/v2/Users/ghost", `{"Operations":[]}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("missing user: status = %d, want 404", w.Code)
	}
}

func TestPutUser_DeactivateAndRename(t *testing.T) {
	r, mock := newSCIM(t)

	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))
	mock.ExpectQuery("DELETE FROM organization_members").WithArgs("u1").
		WillReturnRows(removedOrgRows(1))
	mock.ExpectExec("UPDATE users").
		WithArgs("u1", "renamed@b.c", "Renamed", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"userName":"renamed@b.c","name":{"givenName":"Re","familyName":"named","formatted":"Renamed"},"active":false}`
	w := doJSON(r, http.MethodPut, "/scim/v2/Users/u1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("PUT active=false must deactivate: %v", err)
	}
}

func TestDeleteUser_SoftDeletes(t *testing.T) {
	r, mock := newSCIM(t)

	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
		WillReturnRows(userRow("u1", "a@b.c", "Alice"))
	mock.ExpectQuery("DELETE FROM organization_members").WithArgs("u1").
		WillReturnRows(removedOrgRows(3))

	w := doJSON(r, http.MethodDelete, "/scim/v2/Users/u1", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("delete must only remove memberships (soft): %v", err)
	}
}

func TestDeleteUser_NotFound(t *testing.T) {
	r, mock := newSCIM(t)
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(userCols))
	if w := doJSON(r, http.MethodDelete, "/scim/v2/Users/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGroups(t *testing.T) {
	r, mock := newSCIM(t)
	now := time.Now()
	orgCols := []string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}

	mock.ExpectQuery("FROM organizations").
		WillReturnRows(sqlmock.NewRows(orgCols).AddRow("o1", "default", "Default", nil, nil, now, now))
	w := doJSON(r, http.MethodGet, "/scim/v2/Groups", "")
	if w.Code != http.StatusOK {
		t.Fatalf("ListGroups status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), SchemaGroup) {
		t.Errorf("groups missing SCIM group schema: %s", w.Body.String())
	}

	mock.ExpectQuery("FROM organizations").WithArgs("o1").
		WillReturnRows(sqlmock.NewRows(orgCols).AddRow("o1", "default", "Default", nil, nil, now, now))
	if w := doJSON(r, http.MethodGet, "/scim/v2/Groups/o1", ""); w.Code != http.StatusOK {
		t.Fatalf("GetGroup status = %d (%s)", w.Code, w.Body.String())
	}

	mock.ExpectQuery("FROM organizations").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(orgCols))
	if w := doJSON(r, http.MethodGet, "/scim/v2/Groups/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing group: status = %d, want 404", w.Code)
	}
}
