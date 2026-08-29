package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
)

func newAdminHandlers(t *testing.T) (*AdminHandlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewAdminHandlers(db, nil, approles.RoleSourceIdentity), mock
}

// newAdminHandlersWithApp wires the APP connection to the same sqlmock: the
// role picker and the stats' role count read this application's own
// role_templates now, and there is no identity fallback to serve a rig that
// omits the connection.
func newAdminHandlersWithApp(t *testing.T) (*AdminHandlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewAdminHandlers(db, db, approles.RoleSourceIdentity), mock
}

// serveAdmin runs handler with NO caller in the context. Since identity
// v0.25.0 that is not a neutral rig: callerOrgScope resolves to the empty
// OrgScope, which on audit_logs still admits the org-less platform rows (the
// documented fail-closed degradation) but on organizations matches nothing and
// short-circuits before any query. Use serveAdminAs wherever the handler's
// tenancy is what is under test.
func serveAdmin(handler gin.HandlerFunc, target string) *httptest.ResponseRecorder {
	return serveAdminAs(handler, target, "")
}

// serveAdminAs runs handler with callerID installed the way requireAuth
// installs it, so the caller's organizations resolve.
func serveAdminAs(handler gin.HandlerFunc, target, callerID string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if callerID != "" {
			c.Set("user_id", callerID)
		}
		c.Next()
	})
	r.GET("/x", handler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

// expectCallerAdminOrg queues the membership lookup OrgScopeForUser issues,
// answering "the caller is an admin of orgID".
func expectCallerAdminOrg(mock sqlmock.Sqlmock, callerID, orgID string) {
	mock.ExpectQuery("FROM organization_members om").WithArgs(callerID).
		WillReturnRows(sqlmock.NewRows(userMembershipCols).
			AddRow(orgID, "Org "+orgID, "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`)))
}

func TestPageParams(t *testing.T) {
	tests := []struct {
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"", 50, 0},
		{"?page=1&per_page=25", 25, 0},
		{"?page=3&per_page=10", 10, 20},
		{"?page=0", 50, 0},        // invalid page → 1
		{"?per_page=0", 50, 0},    // invalid per_page → default
		{"?per_page=201", 50, 0},  // above cap → default
		{"?per_page=200", 200, 0}, // at cap
		{"?page=abc&per_page=xyz", 50, 0},
	}
	for _, tt := range tests {
		t.Run("q="+tt.query, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/x"+tt.query, nil)
			limit, offset := pageParams(c)
			if limit != tt.wantLimit || offset != tt.wantOffset {
				t.Errorf("pageParams(%q) = (%d, %d), want (%d, %d)", tt.query, limit, offset, tt.wantLimit, tt.wantOffset)
			}
		})
	}
}

func TestAuditFiltersForUser(t *testing.T) {
	f := auditFiltersForUser("u-1")
	if f.UserID == nil || *f.UserID != "u-1" {
		t.Errorf("UserID filter = %v, want u-1", f.UserID)
	}
}

func TestAuditLogJSON_SnakeCaseShape(t *testing.T) {
	now := time.Now()
	raw, err := json.Marshal(auditLogJSON(&idmodels.AuditLog{ID: "log-1", Action: "user.create", CreatedAt: now}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"id", "user_id", "organization_id", "action", "resource_type",
		"resource_id", "metadata", "ip_address", "created_at", "user_email", "user_name",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing snake_case key %q in audit log JSON", key)
		}
	}
	if m["id"] != "log-1" || m["action"] != "user.create" {
		t.Errorf("values not mapped: %v", m)
	}
}

func TestAuditLogsJSON_EmptyIsNotNull(t *testing.T) {
	out := auditLogsJSON(nil)
	if out == nil {
		t.Fatal("auditLogsJSON(nil) returned nil; clients expect [] not null")
	}
	if len(out) != 0 {
		t.Fatalf("expected empty slice, got %d entries", len(out))
	}
}

func TestAdminStats(t *testing.T) {
	h, mock := newAdminHandlersWithApp(t)
	now := time.Now()

	// Two membership lookups: the user count and the organization count are
	// scoped independently, each matching the list route it mirrors.
	expectCallerAdminOrg(mock, "caller-1", "org-1")
	expectCallerAdminOrg(mock, "caller-1", "org-1")
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	mock.ExpectQuery("SELECT id, name, display_name, idp_type, idp_name, created_at, updated_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}).
			AddRow("org-1", "default", "Default", nil, nil, now, now))
	// The role count reads THIS APPLICATION's own role_templates.
	mock.ExpectQuery(`SELECT id, name, COALESCE\(display_name`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", "admin", "Administrator", "Full access", []byte(`["admin"]`), true, now, now).
			AddRow("22222222-2222-2222-2222-222222222222", "viewer", "Viewer", "Read-only", []byte(`["state:read"]`), true, now, now))

	w := serveAdminAs(h.Stats(), "/x", "caller-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body struct {
		Users         int `json:"users"`
		Organizations int `json:"organizations"`
		Roles         int `json:"roles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Users != 7 || body.Organizations != 1 || body.Roles != 2 {
		t.Errorf("stats = %+v, want users=7 orgs=1 roles=2", body)
	}
}

func TestAdminListRoles(t *testing.T) {
	h, mock := newAdminHandlersWithApp(t)
	now := time.Now()
	mock.ExpectQuery(`SELECT id, name, COALESCE\(display_name`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", "admin", "Administrator", "Full access", []byte(`["admin"]`), true, now, now))

	w := serveAdmin(h.ListRoles(), "/x")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Fatal("response is not valid JSON")
	}
}

func TestAdminListRoles_DBError(t *testing.T) {
	// No expectations queued: any query errors, and the handler must 500.
	h, _ := newAdminHandlersWithApp(t)
	if w := serveAdmin(h.ListRoles(), "/x"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on repository failure", w.Code)
	}
}

func TestAdminListRoles_NoAppConnectionFailsClosed(t *testing.T) {
	// A rig with no application connection gets an error, not the shared
	// identity schema's roles: the fallback was retired with the rest of the
	// identity.role_templates reads.
	h, _ := newAdminHandlers(t)
	if w := serveAdmin(h.ListRoles(), "/x"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 with no application connection", w.Code)
	}
}

func TestAdminListOrganizations(t *testing.T) {
	h, mock := newAdminHandlers(t)
	now := time.Now()
	expectCallerAdminOrg(mock, "caller-1", "org-1")
	mock.ExpectQuery("SELECT id, name, display_name, idp_type, idp_name, created_at, updated_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}).
			AddRow("org-1", "default", "Default", nil, nil, now, now))

	w := serveAdminAs(h.ListOrganizations(), "/x?page=2&per_page=5", "caller-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

func TestAdminListOrganizations_DBError(t *testing.T) {
	h, _ := newAdminHandlers(t)
	// A caller IS installed: without one the scope resolves to "matches nothing"
	// and the repository short-circuits before issuing any statement, so the
	// unqueued-expectation failure this test relies on would never happen.
	if w := serveAdminAs(h.ListOrganizations(), "/x", "caller-1"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on repository failure", w.Code)
	}
}

func TestAdminListUsers_DBError(t *testing.T) {
	h, _ := newAdminHandlers(t)
	if w := serveAdmin(h.ListUsers(), "/x"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on repository failure", w.Code)
	}
}

func TestAdminListUsers_SearchDBError(t *testing.T) {
	h, _ := newAdminHandlers(t)
	if w := serveAdmin(h.ListUsers(), "/x?q=alice"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on repository failure", w.Code)
	}
}

func TestAdminListAuditLogs_DBError(t *testing.T) {
	h, _ := newAdminHandlers(t)
	if w := serveAdmin(h.ListAuditLogs(), "/x"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on repository failure", w.Code)
	}
}

// TestBuildAuditLog_OrgTagging verifies an organization-scoped audit event is
// stamped with its owning org while a platform-level event is left org-less (#298).
func TestBuildAuditLog_OrgTagging(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	c.Set("user_id", "u-1")

	org := buildAuditLog(c, "organization.member.add", "organization", "org-9", "org-9", nil)
	if org.OrganizationID == nil || *org.OrganizationID != "org-9" {
		t.Errorf("OrganizationID = %v, want org-9", org.OrganizationID)
	}
	if org.UserID == nil || *org.UserID != "u-1" {
		t.Errorf("UserID = %v, want u-1", org.UserID)
	}

	plat := buildAuditLog(c, "state.edit", "state", "s-1", "", nil)
	if plat.OrganizationID != nil {
		t.Errorf("platform event OrganizationID = %v, want nil (org-less)", plat.OrganizationID)
	}
}

// The in-memory auditLogsInAdminOrgs filter this file used to cover was
// replaced by the store.AuditScope SQL predicate in identity v0.21.0; its
// semantics are asserted end-to-end, per read axis, in
// admin_audit_scope_test.go.

// TestListRolesWireShapeIsUnchanged pins the JSON GET /admin/roles serves.
//
// The role picker moved off identity.role_templates and onto this application's
// own tables, which changed the Go TYPE the handler serialises — from
// identity/models.RoleTemplate to approles.Template. Those two are field-for-field
// equivalent and tag-for-tag equivalent, and nothing in the compiler or in any
// behavioural test says so: a missing `json:"display_name"` produces a 200 whose
// body says `DisplayName`, and the admin Roles page renders blank cells against a
// successful request. That is this phase's own failure mode arriving through the
// serialiser instead of the query.
//
// The frontend's RoleTemplate interface (frontend/src/services/api.ts) is the
// contract; these are its keys.
func TestListRolesWireShapeIsUnchanged(t *testing.T) {
	h, mock := newAdminHandlersWithApp(t)
	// The picker serves this application's own tables — the only source since
	// the identity fallback was retired; the type-level twin below pins the
	// serialised shape against the identity model it replaced.
	mock.ExpectQuery(`SELECT id, name, COALESCE\(display_name`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow("11111111-0000-0000-0000-000000000001", "editor", "Editor", nil, []byte(`["state:read"]`), true, time.Now(), time.Now()))

	router := gin.New()
	router.GET("/admin/roles", h.ListRoles())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/roles", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Roles []map[string]interface{} `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if len(body.Roles) != 1 {
		t.Fatalf("roles = %v, want one", body.Roles)
	}
	for _, key := range []string{"id", "name", "display_name", "scopes", "is_system", "created_at", "updated_at"} {
		if _, ok := body.Roles[0][key]; !ok {
			t.Errorf("the response has no %q key: the admin Roles page reads it, and its absence is a 200 "+
				"with a blank cell rather than an error. Body: %s", key, rec.Body.String())
		}
	}
}

// TestAppRoleTemplateJSONMatchesTheIdentityShape asserts the TYPE the app leg
// serialises, so the leg that has no sqlmock rig in this package is covered too.
func TestAppRoleTemplateJSONMatchesTheIdentityShape(t *testing.T) {
	desc := "Read and edit state."
	encoded, err := json.Marshal(approles.Template{
		ID: "11111111-0000-0000-0000-000000000001", Name: "editor", DisplayName: "Editor",
		Description: &desc, Scopes: []string{"state:read"}, IsSystem: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("marshalling approles.Template: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, key := range []string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"} {
		if _, ok := got[key]; !ok {
			t.Errorf("approles.Template serialises without %q — the admin Roles page reads it. Got: %s", key, encoded)
		}
	}
	for _, absent := range []string{"DisplayName", "IsSystem", "CreatedAt"} {
		if _, ok := got[absent]; ok {
			t.Errorf("approles.Template serialises the Go field name %q: the json tag is missing", absent)
		}
	}
}
