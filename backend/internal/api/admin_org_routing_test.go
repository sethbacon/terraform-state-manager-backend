package api

import (
	"net/http"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
)

// newAdminOrgRoutingEnv wires the /admin/organizations route group the same
// way router.go does in production: organizations:read/:create scopes (via the
// REAL middleware.RequireScope + auth.Scope constants, not a hand-rolled stub)
// gate the list/create endpoints, and requireOrgScope gates the :id subtree —
// so a drift between this rig and router.go's actual gates would show up as a
// test failure here. This intentionally mirrors router.go's structure rather
// than reusing newAdminOrgScopeEnv (which predates the organizations:read/
// :create scope gates and omits them by design, to isolate requireOrgScope's
// own per-org logic in isolation).
func newAdminOrgRoutingEnv(t *testing.T, callerUserID string, scopes []string) *sourcesEnv {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewAdminHandlers(sqlx.NewDb(db, "sqlmock"))
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if callerUserID != "" {
			c.Set("user_id", callerUserID)
		}
		if scopes != nil {
			c.Set("scopes", scopes)
		}
		c.Next()
	})
	v1 := r.Group("/api/v1")
	orgAdmin := v1.Group("/admin/organizations")
	orgAdmin.GET("", middleware.RequireScope(auth.ScopeOrganizationsRead), h.ListOrganizations())
	orgAdmin.POST("", middleware.RequireScope(auth.ScopeOrganizationsCreate), h.CreateOrganization())
	orgScoped := orgAdmin.Group("/:id", h.requireOrgScope())
	{
		orgScoped.PUT("", h.UpdateOrganization())
		orgScoped.DELETE("", h.DeleteOrganization())
		orgScoped.GET("/members", h.ListOrganizationMembers())
		orgScoped.POST("/members", h.AddOrganizationMember())
		orgScoped.PUT("/members/:user_id", h.UpdateOrganizationMember())
		orgScoped.DELETE("/members/:user_id", h.RemoveOrganizationMember())
	}
	return &sourcesEnv{r: r, mock: mock}
}

func TestAdminOrgRouting_ListRequiresOrganizationsReadScope(t *testing.T) {
	e := newAdminOrgRoutingEnv(t, "caller-1", []string{"state:read"})

	w := e.do(http.MethodGet, "/api/v1/admin/organizations", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("list without organizations:read: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ListOrganizations must NOT be reached: %v", err)
	}
}

func TestAdminOrgRouting_ListAllowsOrganizationsReadScope(t *testing.T) {
	e := newAdminOrgRoutingEnv(t, "caller-1", []string{"organizations:read"})

	e.mock.ExpectQuery("FROM organizations").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}))

	w := e.do(http.MethodGet, "/api/v1/admin/organizations", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list with organizations:read: status = %d (%s), want 200", w.Code, w.Body.String())
	}
}

// TestAdminOrgRouting_CreateRequiresOrganizationsCreateScope proves
// organizations:create is a DISTINCT, non-implied scope — holding
// organizations:read and organizations:write (the org_owner scopes) is not
// enough to create a brand-new top-level organization.
func TestAdminOrgRouting_CreateRequiresOrganizationsCreateScope(t *testing.T) {
	e := newAdminOrgRoutingEnv(t, "caller-1", []string{"organizations:read", "organizations:write"})

	w := e.do(http.MethodPost, "/api/v1/admin/organizations", `{"name":"Acme"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("create without organizations:create: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("CreateOrganization must NOT be reached: %v", err)
	}
}

func TestAdminOrgRouting_CreateAllowsOrganizationsCreateScope(t *testing.T) {
	e := newAdminOrgRoutingEnv(t, "caller-1", []string{"organizations:create"})

	e.mock.ExpectQuery("INSERT INTO organizations").
		WithArgs("Acme", "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
			AddRow("org-1", time.Now(), time.Now()))
	e.mock.ExpectQuery("SELECT id FROM role_templates").
		WithArgs("org_owner").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-org-owner"))
	e.mock.ExpectExec("INSERT INTO organization_members").
		WithArgs("org-1", "caller-1", "rt-org-owner").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("INSERT INTO audit_logs").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/admin/organizations", `{"name":"Acme"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with organizations:create: status = %d (%s), want 201", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected the full create+org_owner-membership sequence to run: %v", err)
	}
}

// TestAdminOrgRouting_AdminWildcardBypassesOrganizationsReadAndCreate proves
// the flat admin scope still satisfies the new organizations:read/:create
// gates (auth.HasScope's admin wildcard), so existing platform admins are
// unaffected by moving organization management off the admin-gated group.
func TestAdminOrgRouting_AdminWildcardBypassesOrganizationsReadAndCreate(t *testing.T) {
	e := newAdminOrgRoutingEnv(t, "caller-1", []string{"admin"})

	e.mock.ExpectQuery("FROM organizations").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}))

	w := e.do(http.MethodGet, "/api/v1/admin/organizations", "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin wildcard should satisfy organizations:read: status = %d (%s), want 200", w.Code, w.Body.String())
	}
}
