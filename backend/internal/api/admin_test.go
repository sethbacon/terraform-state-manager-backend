package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
)

func newAdminHandlers(t *testing.T) (*AdminHandlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewAdminHandlers(sqlx.NewDb(db, "sqlmock")), mock
}

func serveAdmin(handler gin.HandlerFunc, target string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", handler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
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
	h, mock := newAdminHandlers(t)
	now := time.Now()

	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	mock.ExpectQuery("SELECT id, name, display_name, idp_type, idp_name, created_at, updated_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}).
			AddRow("org-1", "default", "Default", nil, nil, now, now))
	mock.ExpectQuery("SELECT id, name, display_name, description, scopes, is_system, created_at, updated_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "description", "scopes", "is_system", "created_at", "updated_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", "admin", "Administrator", "Full access", []byte(`["admin"]`), true, now, now).
			AddRow("22222222-2222-2222-2222-222222222222", "viewer", "Viewer", "Read-only", []byte(`["state:read"]`), true, now, now))

	w := serveAdmin(h.Stats(), "/x")
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
	h, mock := newAdminHandlers(t)
	now := time.Now()
	mock.ExpectQuery("SELECT id, name, display_name, description, scopes, is_system, created_at, updated_at").
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
	h, _ := newAdminHandlers(t)
	if w := serveAdmin(h.ListRoles(), "/x"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on repository failure", w.Code)
	}
}

func TestAdminListOrganizations(t *testing.T) {
	h, mock := newAdminHandlers(t)
	now := time.Now()
	mock.ExpectQuery("SELECT id, name, display_name, idp_type, idp_name, created_at, updated_at").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}).
			AddRow("org-1", "default", "Default", nil, nil, now, now))

	w := serveAdmin(h.ListOrganizations(), "/x?page=2&per_page=5")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

func TestAdminListOrganizations_DBError(t *testing.T) {
	h, _ := newAdminHandlers(t)
	if w := serveAdmin(h.ListOrganizations(), "/x"); w.Code != http.StatusInternalServerError {
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

// TestAuditLogsInAdminOrgs verifies the admin audit view keeps org-less platform
// events and the caller's own-org events while filtering out another org's
// entries (#298/#182).
func TestAuditLogsInAdminOrgs(t *testing.T) {
	orgless := &idmodels.AuditLog{ID: "l0", Action: "state.edit"} // OrganizationID nil
	myOrg, otherOrg := "org-a", "org-b"
	mine := &idmodels.AuditLog{ID: "l1", Action: "organization.member.add", OrganizationID: &myOrg}
	other := &idmodels.AuditLog{ID: "l2", Action: "organization.member.add", OrganizationID: &otherOrg}

	got := auditLogsInAdminOrgs([]*idmodels.AuditLog{orgless, mine, other}, map[string]struct{}{"org-a": {}})

	if len(got) != 2 {
		t.Fatalf("kept %d entries, want 2 (org-less + own-org)", len(got))
	}
	kept := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !kept["l0"] || !kept["l1"] {
		t.Errorf("kept %v, want the org-less (l0) and own-org (l1) entries", kept)
	}
	if kept["l2"] {
		t.Error("another org's entry (l2) must be filtered out")
	}
}
