package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/credlifecycle"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
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

	// The credential sweeper is wired exactly as the router wires it, so the
	// offboarding routes are exercised on their production path (#330).
	h := NewAdminHandlers(db, WithAdminCredentialSweeper(
		credlifecycle.NewSweeper(
			repositories.NewUserTokenRevocationRepository(db),
			idstore.NewAPIKeyRepository(db),
			idstore.NewOrganizationRepository(db))))
	r := gin.New()
	admin := r.Group("/api/v1/admin")
	admin.POST("/users", h.CreateUser())
	admin.PUT("/users/:id", h.UpdateUser())
	admin.DELETE("/users/:id", h.DeleteUser())
	// Only the routes whose handlers resolve a CALLER scope get an
	// authenticated caller here. Setting it router-wide changed behaviour for
	// unrelated handlers (self-deletion guards and the like), which is its own
	// kind of wrong. Production puts a real authenticated principal on all of
	// them; these tests only need the ones under test.
	authed := func(c *gin.Context) { c.Set("user_id", "caller-1") }
	admin.GET("/users/:id/memberships", authed, h.GetUserMemberships())
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
	e.mock.ExpectQuery("INSERT INTO audit_logs").WillReturnRows(auditInsertReturn())
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
	// Both credential families are invalidated before the account is removed:
	// the revoke-all watermark retires live sessions, then the key rows go
	// (none here).
	e.mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Since identity v0.25.0 the key sweep is ONE bulk DELETE rather than a list
	// followed by a delete per row, so there is no window in which a key minted
	// mid-sweep survives it.
	e.mock.ExpectExec("DELETE FROM api_keys").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 0))
	e.mock.ExpectExec("DELETE FROM users").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/admin/users/u1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w.Code)
	}
}

// apiKeyListCols mirrors ListAPIKeysByUser's scanAPIKeyWithUserName projection.
var apiKeyListCols = []string{
	"id", "user_id", "organization_id", "name", "description", "key_hash", "key_prefix", "scopes",
	"expires_at", "last_used_at", "expiry_notification_sent_at", "created_at", "user_name",
}

func TestAdminDeleteUserRevokesAPIKeys(t *testing.T) {
	e := newAdminWriteEnv(t)
	// The offboarded user owns two keys; both must be deleted before the account,
	// and their live sessions retired with them. The sweep is keyed on the OWNER
	// and reaches every organization, which is the point: a TSM key carries the
	// default organization's id whoever owns it, so an org-scoped sweep would
	// strand exactly the credentials this route exists to destroy.
	e.mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM api_keys").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 2))
	e.mock.ExpectExec("DELETE FROM users").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))

	if w := e.do(http.MethodDelete, "/api/v1/admin/users/u1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d", w.Code)
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("both keys must be revoked before the user is deleted: %v", err)
	}
}

var membershipCols = []string{"organization_id", "organization_name", "role_template_id", "created_at",
	"role_template_name", "role_template_display_name", "role_template_scopes"}

func TestAdminGetUserMemberships(t *testing.T) {
	e := newAdminWriteEnv(t)
	// The caller's own memberships, resolving the scope the response is
	// filtered against (identity #183). Admin in o1, so o1 is visible.
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("caller-1").
		WillReturnRows(sqlmock.NewRows(membershipCols).
			AddRow("o1", "default", "rt-1", time.Now(), "admin", "Admin", []byte(`["admin"]`)))
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

// auditCols mirrors ListAuditLogs' projection; actor_email is column 10 as of
// identity v0.25.0 (see auditRowCols in admin_audit_export_test.go).
var auditCols = []string{"id", "user_id", "organization_id", "action", "resource_type", "resource_id",
	"metadata", "ip_address", "created_at", "actor_email", "user_email", "user_name"}

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
			AddRow("l1", "u1", nil, "state.edit", "state", "s1", nil, "127.0.0.1", time.Now(), "a@b.c", "a@b.c", "Alice"))

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
	// The membership strip now RETURNS the organizations it emptied (an OrgScope
	// since v0.25.0, not a count), so it is a query rather than an exec.
	e.mock.ExpectQuery("DELETE FROM organization_members").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("o1").AddRow("o2"))
	// Erasure also revokes the user's sessions and API keys (the tombstone would
	// otherwise keep both valid), in one bulk delete.
	e.mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM api_keys").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/admin/users/u1/erase", "")
	if w.Code != http.StatusOK {
		t.Fatalf("erase: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("erase must anonymize, revoke memberships, and revoke api keys: %v", err)
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
	e.mock.ExpectQuery("FROM organizations").WithArgs("o1", pq.Array([]string{"o1"})).
		WillReturnRows(sqlmock.NewRows(orgCols).AddRow("o1", "eng", "Engineering", nil, nil, now, now))
	e.mock.ExpectExec("UPDATE organizations SET name").WithArgs("platform", "o1", pq.Array([]string{"o1"})).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("UPDATE organizations").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPut, "/api/v1/admin/organizations/o1",
		`{"name":"platform","display_name":"Platform","idp_type":"oidc","idp_name":"keycloak"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "platform") {
		t.Fatalf("update org: status = %d (%s)", w.Code, w.Body.String())
	}

	// Clearing the IdP binding with empty strings.
	e.mock.ExpectQuery("FROM organizations").WithArgs("o1", pq.Array([]string{"o1"})).
		WillReturnRows(sqlmock.NewRows(orgCols).AddRow("o1", "platform", "Platform", "oidc", "keycloak", now, now))
	e.mock.ExpectExec("UPDATE organizations").WillReturnResult(sqlmock.NewResult(0, 1))
	w = e.do(http.MethodPut, "/api/v1/admin/organizations/o1", `{"idp_type":"","idp_name":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear idp: status = %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "keycloak") {
		t.Error("empty idp fields must clear the binding")
	}

	e.mock.ExpectQuery("FROM organizations").WithArgs("ghost", pq.Array([]string{"ghost"})).WillReturnRows(sqlmock.NewRows(orgCols))
	if w := e.do(http.MethodPut, "/api/v1/admin/organizations/ghost", `{"name":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("missing org: status = %d, want 404", w.Code)
	}

	// Members are snapshotted BEFORE the delete: organization_members cascades,
	// so afterwards there is nobody left to sweep (none here).
	e.mock.ExpectQuery("FROM organization_members").WithArgs("o1", pq.Array([]string{"o1"})).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id", "user_id", "role_template_id", "created_at"}))
	e.mock.ExpectExec("DELETE FROM organizations").WithArgs("o1", pq.Array([]string{"o1"})).WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/admin/organizations/o1", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete org: status = %d, want 204", w.Code)
	}
}

func TestAdminOrganizationMembers(t *testing.T) {
	e := newAdminWriteEnv(t)

	memberWithUserCols := []string{"organization_id", "user_id", "role_template_id", "created_at",
		"user_name", "user_email", "role_template_name", "role_template_display_name", "role_template_scopes"}
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("o1", pq.Array([]string{"o1"})).
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

	e.mock.ExpectExec("DELETE FROM organization_members").WithArgs("o1", "u1", pq.Array([]string{"o1"})).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if w := e.do(http.MethodDelete, "/api/v1/admin/organizations/o1/members/u1", ""); w.Code != http.StatusNoContent {
		t.Errorf("remove member: status = %d, want 204", w.Code)
	}
}

// TestAdminEraseUser_StripsMembershipsInEveryOrganization pins the one tenancy
// decision on this route that is deliberately NOT narrowed.
//
// GDPR Article 17 is an obligation about the whole data subject, and the steps
// either side of the strip — anonymizing the users row and revoking every
// credential — are already whole-principal. Narrowing the strip to the caller's
// administered organizations would leave the "erased" account a live member
// elsewhere, with an authority the anonymized row can still exercise: a
// compliance failure AND a live-access failure, produced by a change that looks
// like tightening.
//
// It is asserted on the emitted predicate, not on the bound arguments, because
// that is the only place the difference shows: the caller-scoped variant binds a
// predicate over organization_id, while the platform-wide one is a literal TRUE.
// A caller-less test rig would match both on WithArgs alone.
func TestAdminEraseUser_StripsMembershipsInEveryOrganization(t *testing.T) {
	rec := &auditSQLRecorder{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
			rec.record(actualSQL)
			return sqlmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
		}),
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewAdminHandlers(db, WithAdminCredentialSweeper(
		credlifecycle.NewSweeper(
			repositories.NewUserTokenRevocationRepository(db),
			idstore.NewAPIKeyRepository(db),
			idstore.NewOrganizationRepository(db))))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", "caller-1"); c.Next() })
	r.POST("/users/:id/erase", h.EraseUser())

	// The caller administers ONE organization; the subject belongs to two.
	mock.ExpectQuery("FROM organization_members om").WithArgs("caller-1").
		WillReturnRows(sqlmock.NewRows(userMembershipCols).
			AddRow("org-a", "Org A", "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`)))
	mock.ExpectQuery("SELECT id, email, name, oidc_sub").
		WithArgs("u1", pq.Array([]string{"org-a"})).WillReturnRows(idUserRow("u1"))
	mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("DELETE FROM organization_members").WithArgs("u1").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("org-a").AddRow("org-b"))
	mock.ExpectExec("INSERT INTO user_token_revocations").WithArgs("u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM api_keys").WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/users/u1/erase", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("erase: status = %d (%s)", w.Code, w.Body.String())
	}

	var strip string
	for _, q := range rec.seen {
		if strings.Contains(q, "DELETE FROM organization_members") {
			strip = q
		}
	}
	if strip == "" {
		t.Fatal("no membership strip reached the database")
	}
	if !strings.Contains(strip, "AND TRUE") {
		t.Errorf("GDPR erasure must strip memberships in EVERY organization, not just the "+
			"caller's. A tenant predicate here leaves the erased account a live member "+
			"elsewhere.\ngot statement: %s", strip)
	}
	// ...and the response reports where authority was actually withdrawn, which
	// the pre-v0.25.0 int64 count could not.
	if !strings.Contains(w.Body.String(), "u1") {
		t.Errorf("erase response should name the subject: %s", w.Body.String())
	}
}
