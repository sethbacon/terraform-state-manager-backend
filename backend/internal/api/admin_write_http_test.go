package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// newAdminWriteEnv wires AdminHandlers' write routes over a sqlmock identity DB.
// Audit writes are best-effort (failures logged, never blocking), so tests only
// queue audit-INSERT expectations where they assert auditing happened.
func newAdminWriteEnv(t *testing.T) *sourcesEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewAdminHandlers(sqlx.NewDb(db, "sqlmock"))
	r := gin.New()
	admin := r.Group("/api/v1/admin")
	admin.POST("/users", h.CreateUser())
	admin.PUT("/users/:id", h.UpdateUser())
	admin.DELETE("/users/:id", h.DeleteUser())
	admin.GET("/users/:id/memberships", h.GetUserMemberships())
	admin.GET("/users/:id/export", h.ExportUserData())
	admin.POST("/users/:id/erase", h.EraseUser())
	admin.POST("/organizations", h.CreateOrganization())
	admin.PUT("/organizations/:id", h.UpdateOrganization())
	admin.DELETE("/organizations/:id", h.DeleteOrganization())
	admin.GET("/organizations/:id/members", h.ListOrganizationMembers())
	admin.POST("/organizations/:id/members", h.AddOrganizationMember())
	admin.PUT("/organizations/:id/members/:user_id", h.UpdateOrganizationMember())
	admin.DELETE("/organizations/:id/members/:user_id", h.RemoveOrganizationMember())
	return &sourcesEnv{r: r, mock: mock}
}

var idUserCols = []string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}

func idUserRow(id string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(idUserCols).AddRow(id, "a@b.c", "Alice", "sub-1", now, now)
}

func TestAdminCreateUser(t *testing.T) {
	e := newAdminWriteEnv(t)

	if w := e.do(http.MethodPost, "/api/v1/admin/users", `{"name":"no email"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing email: status = %d, want 400", w.Code)
	}

	e.mock.ExpectExec("INSERT INTO users").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("INSERT INTO audit_logs").WillReturnResult(sqlmock.NewResult(0, 1))
	w := e.do(http.MethodPost, "/api/v1/admin/users", `{"email":" a@b.c ","name":"Alice"}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"a@b.c"`) {
		t.Fatalf("create: status = %d (%s) — email should be trimmed", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("user.create must be audited: %v", err)
	}
}

func TestAdminUpdateUser(t *testing.T) {
	e := newAdminWriteEnv(t)

	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	if w := e.do(http.MethodPut, "/api/v1/admin/users/ghost", `{"name":"X"}`); w.Code != http.StatusNotFound {
		t.Errorf("missing user: status = %d, want 404", w.Code)
	}

	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").WillReturnRows(idUserRow("u1"))
	e.mock.ExpectExec("UPDATE users").
		WithArgs("u1", "a@b.c", "Renamed", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	w := e.do(http.MethodPut, "/api/v1/admin/users/u1", `{"name":" Renamed "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: status = %d (%s)", w.Code, w.Body.String())
	}
}

func TestAdminDeleteUser(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectExec("DELETE FROM users").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/admin/users/u1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w.Code)
	}
}

var membershipCols = []string{"organization_id", "organization_name", "role_template_id", "created_at",
	"role_template_name", "role_template_display_name", "role_template_scopes"}

func TestAdminGetUserMemberships(t *testing.T) {
	e := newAdminWriteEnv(t)
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, time.Now(), nil, nil, []byte(`[]`)))
	w := e.do(http.MethodGet, "/api/v1/admin/users/u1/memberships", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"default"`) {
		t.Fatalf("memberships: status = %d (%s)", w.Code, w.Body.String())
	}

	if w := e.do(http.MethodGet, "/api/v1/admin/users/u1/memberships", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("DB error: status = %d, want 500", w.Code)
	}
}

var auditCols = []string{"id", "user_id", "organization_id", "action", "resource_type", "resource_id",
	"metadata", "ip_address", "created_at", "user_email", "user_name"}

func TestAdminExportUserData(t *testing.T) {
	e := newAdminWriteEnv(t)

	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows(idUserCols))
	if w := e.do(http.MethodGet, "/api/v1/admin/users/ghost/export", ""); w.Code != http.StatusNotFound {
		t.Errorf("missing user: status = %d, want 404", w.Code)
	}

	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").WillReturnRows(idUserRow("u1"))
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", nil, time.Now(), nil, nil, []byte(`[]`)))
	e.mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	e.mock.ExpectQuery("FROM audit_logs al").
		WillReturnRows(sqlmock.NewRows(auditCols).
			AddRow("l1", "u1", nil, "state.edit", "state", "s1", nil, "127.0.0.1", time.Now(), "a@b.c", "Alice"))

	w := e.do(http.MethodGet, "/api/v1/admin/users/u1/export", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "user-data-u1.json") {
		t.Errorf("export filename wrong: %q", w.Header().Get("Content-Disposition"))
	}
	body := w.Body.String()
	for _, want := range []string{`"exported_at"`, `"memberships"`, `"audit_logs"`, `"state.edit"`} {
		if !strings.Contains(body, want) {
			t.Errorf("export missing %s", want)
		}
	}
}

func TestAdminEraseUser(t *testing.T) {
	e := newAdminWriteEnv(t)

	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").WillReturnRows(idUserRow("u1"))
	// Anonymization must rewrite PII and clear the OIDC link (nil sub).
	e.mock.ExpectExec("UPDATE users").
		WithArgs("u1", "erased-u1@anonymized.invalid", "Erased User", nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM organization_members").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 2))

	w := e.do(http.MethodPost, "/api/v1/admin/users/u1/erase", "")
	if w.Code != http.StatusOK {
		t.Fatalf("erase: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("erase must anonymize and revoke memberships: %v", err)
	}
}

func TestAdminOrganizationCRUD(t *testing.T) {
	e := newAdminWriteEnv(t)
	now := time.Now()

	if w := e.do(http.MethodPost, "/api/v1/admin/organizations", `{"display_name":"X"}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing name: status = %d, want 400", w.Code)
	}

	e.mock.ExpectQuery("INSERT INTO organizations").WithArgs("eng", "Engineering").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow("o1", now, now))
	w := e.do(http.MethodPost, "/api/v1/admin/organizations", `{"name":" eng ","display_name":"Engineering"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create org: status = %d (%s)", w.Code, w.Body.String())
	}

	orgCols := []string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}
	// Update: rename + bind IdP.
	e.mock.ExpectQuery("FROM organizations").WithArgs("o1").
		WillReturnRows(sqlmock.NewRows(orgCols).AddRow("o1", "eng", "Engineering", nil, nil, now, now))
	e.mock.ExpectExec("UPDATE organizations SET name").WithArgs("platform", "o1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE organizations").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPut, "/api/v1/admin/organizations/o1",
		`{"name":"platform","display_name":"Platform","idp_type":"oidc","idp_name":"keycloak"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "platform") {
		t.Fatalf("update org: status = %d (%s)", w.Code, w.Body.String())
	}

	// Clearing the IdP binding with empty strings.
	e.mock.ExpectQuery("FROM organizations").WithArgs("o1").
		WillReturnRows(sqlmock.NewRows(orgCols).AddRow("o1", "platform", "Platform", "oidc", "keycloak", now, now))
	e.mock.ExpectExec("UPDATE organizations").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPut, "/api/v1/admin/organizations/o1", `{"idp_type":"","idp_name":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear idp: status = %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "keycloak") {
		t.Error("empty idp fields must clear the binding")
	}

	e.mock.ExpectQuery("FROM organizations").WithArgs("ghost").WillReturnRows(sqlmock.NewRows(orgCols))
	if w := e.do(http.MethodPut, "/api/v1/admin/organizations/ghost", `{"name":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("missing org: status = %d, want 404", w.Code)
	}

	e.mock.ExpectExec("DELETE FROM organizations").WithArgs("o1").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/admin/organizations/o1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete org: status = %d, want 204", w.Code)
	}
}

func TestAdminOrganizationMembers(t *testing.T) {
	e := newAdminWriteEnv(t)

	memberWithUserCols := []string{"organization_id", "user_id", "role_template_id", "created_at",
		"user_name", "user_email", "role_template_name", "role_template_display_name", "role_template_scopes"}
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("o1").
		WillReturnRows(sqlmock.NewRows(memberWithUserCols).
			AddRow("o1", "u1", nil, time.Now(), "Alice", "a@b.c", nil, nil, []byte(`[]`)))
	w := e.do(http.MethodGet, "/api/v1/admin/organizations/o1/members", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"a@b.c"`) {
		t.Fatalf("list members: status = %d (%s)", w.Code, w.Body.String())
	}

	if w := e.do(http.MethodPost, "/api/v1/admin/organizations/o1/members", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing user_id: status = %d, want 400", w.Code)
	}
	if w := e.do(http.MethodPost, "/api/v1/admin/organizations/o1/members",
		`{"user_id":"u1","role_template_id":"not-a-uuid"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad role id: status = %d, want 400", w.Code)
	}

	roleTemplateCols := []string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}
	e.mock.ExpectQuery("FROM role_templates WHERE").
		WithArgs("6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0").
		WillReturnRows(sqlmock.NewRows(roleTemplateCols).
			AddRow("6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0", "viewer", "Viewer", "read-only", []byte(`[]`), false, time.Now(), time.Now()))
	e.mock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPost, "/api/v1/admin/organizations/o1/members",
		`{"user_id":"u1","role_template_id":"6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("add member: status = %d (%s)", w.Code, w.Body.String())
	}

	e.mock.ExpectQuery("FROM role_templates WHERE").
		WithArgs("6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0").
		WillReturnRows(sqlmock.NewRows(roleTemplateCols).
			AddRow("6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0", "viewer", "Viewer", "read-only", []byte(`[]`), false, time.Now(), time.Now()))
	e.mock.ExpectExec("UPDATE organization_members").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPut, "/api/v1/admin/organizations/o1/members/u1",
		`{"role_template_id":"6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update member: status = %d (%s)", w.Code, w.Body.String())
	}

	e.mock.ExpectExec("DELETE FROM organization_members").WithArgs("o1", "u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/admin/organizations/o1/members/u1", ""); w.Code != http.StatusNoContent {
		t.Errorf("remove member: status = %d, want 204", w.Code)
	}
}
