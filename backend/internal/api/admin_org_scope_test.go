package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// newAdminOrgScopeEnv wires the /admin/organizations/:id* routes the same way
// router.go does: a stand-in for requireAuth sets user_id in the gin context
// (the real middleware chain populates this from the validated JWT), then
// requireOrgScope gates the :id-scoped routes ahead of the handler.
func newAdminOrgScopeEnv(t *testing.T, callerUserID string) *sourcesEnv {
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
		c.Next()
	})
	admin := r.Group("/api/v1/admin")
	admin.GET("/organizations", h.ListOrganizations())
	admin.POST("/organizations", h.CreateOrganization())
	orgScoped := admin.Group("/organizations/:id", h.requireOrgScope())
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

// scopeMemberCols mirrors GetMemberWithRole/ListMembersWithUsers' row shape
// (identity module's organization_repository.go).
var scopeMemberCols = []string{"organization_id", "user_id", "role_template_id", "created_at",
	"user_name", "user_email", "role_template_name", "role_template_display_name", "role_template_scopes"}

// expectGetUserScopesForOrg queues the GetUserScopesForOrg lookup requireOrgScope
// issues, returning a membership row carrying roleScopesJSON (e.g. `["admin"]`).
func expectGetUserScopesForOrg(mock sqlmock.Sqlmock, orgID, userID string, roleScopesJSON string) {
	mock.ExpectQuery("FROM organization_members om").
		WithArgs(orgID, userID).
		WillReturnRows(sqlmock.NewRows(scopeMemberCols).
			AddRow(orgID, userID, "rt-1", time.Now(), "Caller", "caller@example.com", "role", "Role", []byte(roleScopesJSON)))
}

// expectNoMembership queues a GetUserScopesForOrg lookup that finds no
// membership row at all for the caller in orgID (empty result set).
func expectNoMembership(mock sqlmock.Sqlmock, orgID, userID string) {
	mock.ExpectQuery("FROM organization_members om").
		WithArgs(orgID, userID).
		WillReturnRows(sqlmock.NewRows(scopeMemberCols))
}

func TestRequireOrgScope_AllowsAdminActingOnOwnOrg(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["admin"]`)
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("org-a").
		WillReturnRows(sqlmock.NewRows(scopeMemberCols))

	w := e.do(http.MethodGet, "/api/v1/admin/organizations/org-a/members", "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin of org-a acting on org-a: status = %d (%s), want 200", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected the membership check then the members list to run: %v", err)
	}
}

func TestRequireOrgScope_RejectsCrossOrgAdmin_NoMembership(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	// Caller is admin in org-a (established by the outer /admin ScopeAdmin gate
	// in production; irrelevant to this handler-level test) but holds no
	// membership at all in org-b, the target of this request.
	expectNoMembership(e.mock, "org-b", "caller-1")

	w := e.do(http.MethodGet, "/api/v1/admin/organizations/org-b/members", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-org admin with no membership in target org: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("membership check must run and the handler must NOT be reached: %v", err)
	}
}

func TestRequireOrgScope_RejectsCrossOrgAdmin_NonAdminRoleInTargetOrg(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	// Caller has SOME membership in org-b, but only a viewer role there — not
	// admin. The flat/global scope check the outer /admin group already passed
	// must not be trusted as a stand-in for this.
	expectGetUserScopesForOrg(e.mock, "org-b", "caller-1", `["state:read"]`)

	w := e.do(http.MethodDelete, "/api/v1/admin/organizations/org-b", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin membership in target org: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DeleteOrganization must NOT be reached: %v", err)
	}
}

func TestRequireOrgScope_AllowsAdminForMemberMutations(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["admin"]`)
	e.mock.ExpectExec("DELETE FROM organization_members").WithArgs("org-a", "u2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodDelete, "/api/v1/admin/organizations/org-a/members/u2", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin of org-a removing a member of org-a: status = %d (%s), want 204", w.Code, w.Body.String())
	}
}

func TestRequireOrgScope_RejectsCrossOrgForMemberMutations(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	expectNoMembership(e.mock, "org-b", "caller-1")

	w := e.do(http.MethodDelete, "/api/v1/admin/organizations/org-b/members/u2", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-org member removal: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("RemoveOrganizationMember must NOT be reached: %v", err)
	}
}

func TestRequireOrgScope_DBErrorIsInternalServerError(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	e.mock.ExpectQuery("FROM organization_members om").WithArgs("org-a", "caller-1").
		WillReturnError(errors.New("db down"))

	w := e.do(http.MethodGet, "/api/v1/admin/organizations/org-a/members", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("membership-check DB error: status = %d (%s), want 500", w.Code, w.Body.String())
	}
}

func TestRequireOrgScope_MissingCallerIdentityIsForbidden(t *testing.T) {
	// No user_id in context at all — simulates a misconfigured chain rather than
	// a normal request (requireAuth always sets it), but must fail closed.
	e := newAdminOrgScopeEnv(t, "")

	w := e.do(http.MethodGet, "/api/v1/admin/organizations/org-a/members", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing caller identity: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no DB query should run without a caller id: %v", err)
	}
}

func TestRequireOrgScope_DoesNotGateOrganizationListOrCreate(t *testing.T) {
	// /admin/organizations (list/create) names no specific target organization,
	// so it must stay gated only by the outer /admin ScopeAdmin check (exercised
	// by the router's own middleware chain in production, not this handler-level
	// rig) — requireOrgScope must not be on its path at all.
	e := newAdminOrgScopeEnv(t, "caller-1")

	e.mock.ExpectQuery("FROM organizations").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}))
	w := e.do(http.MethodGet, "/api/v1/admin/organizations", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list organizations: status = %d (%s), want 200 (no membership check expected)", w.Code, w.Body.String())
	}
}
