package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
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
	admin.GET("/users", h.ListUsers())
	admin.POST("/users", h.CreateUser())
	userScoped := admin.Group("/users/:id", h.requireSharedOrgAdminWithTargetUser())
	{
		userScoped.PUT("", h.UpdateUser())
		userScoped.DELETE("", h.DeleteUser())
		userScoped.GET("/memberships", h.GetUserMemberships())
		userScoped.GET("/export", h.ExportUserData())
		userScoped.POST("/erase", h.EraseUser())
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

// userMembershipCols mirrors GetUserMemberships' row shape (identity module's
// organization_repository.go).
var userMembershipCols = []string{"organization_id", "organization_name", "role_template_id", "created_at",
	"role_template_name", "role_template_display_name", "role_template_scopes"}

// expectGetUserMemberships queues the GetUserMemberships lookup
// requireSharedOrgAdminWithTargetUser issues for the TARGET user, returning one
// membership row per orgID given (empty orgIDs -> a no-memberships result).
func expectGetUserMemberships(mock sqlmock.Sqlmock, targetUserID string, orgIDs ...string) {
	rows := sqlmock.NewRows(userMembershipCols)
	for _, orgID := range orgIDs {
		rows.AddRow(orgID, "Org "+orgID, "rt-1", time.Now(), "role", "Role", []byte(`["placeholder"]`))
	}
	mock.ExpectQuery("FROM organization_members om").WithArgs(targetUserID).WillReturnRows(rows)
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
	// so requireOrgScope must not be on its path at all — in production these are
	// gated instead by organizations:read/:create scopes on a sibling route group
	// (see router.go and admin_org_routing_test.go), which this handler-level rig
	// intentionally omits so it can isolate requireOrgScope's own behavior.
	e := newAdminOrgScopeEnv(t, "caller-1")

	e.mock.ExpectQuery("FROM organizations").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "name", "display_name", "idp_type", "idp_name", "created_at", "updated_at"}))
	w := e.do(http.MethodGet, "/api/v1/admin/organizations", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list organizations: status = %d (%s), want 200 (no membership check expected)", w.Code, w.Body.String())
	}
}

// TestCreateOrganization_AddsCallerAsOrgOwner covers the bootstrap fix: without
// auto-adding the creator as the new organization's org_owner member, no
// caller could ever pass requireOrgScope for a brand-new organization (it has
// zero members), so nobody could manage it — not even its creator. org_owner
// (rather than admin) is what keeps this from re-granting the flat platform
// admin wildcard just for creating an organization.
func TestCreateOrganization_AddsCallerAsOrgOwner(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	e.mock.ExpectQuery("INSERT INTO organizations").
		WithArgs("Acme", "Acme Corp").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
			AddRow("org-1", time.Now(), time.Now()))
	e.mock.ExpectQuery("SELECT id FROM role_templates").
		WithArgs("org_owner").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-org-owner"))
	e.mock.ExpectExec("INSERT INTO organization_members").
		WithArgs("org-1", "caller-1", "rt-org-owner").
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("INSERT INTO audit_logs").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/admin/organizations", `{"name":"Acme","display_name":"Acme Corp"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create organization: status = %d (%s), want 201", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("creator must be added as an org_owner member of the new organization: %v", err)
	}
}

// TestCreateOrganization_FailsIfAddingOrgOwnerMemberFails ensures a failure to
// add the creator as org_owner is surfaced as an error (rather than silently
// returning 201 for an organization nobody can manage).
func TestCreateOrganization_FailsIfAddingOrgOwnerMemberFails(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	e.mock.ExpectQuery("INSERT INTO organizations").
		WithArgs("Acme", "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
			AddRow("org-1", time.Now(), time.Now()))
	e.mock.ExpectQuery("SELECT id FROM role_templates").
		WithArgs("org_owner").
		WillReturnError(errors.New("role template not seeded"))

	w := e.do(http.MethodPost, "/api/v1/admin/organizations", `{"name":"Acme"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("create organization: status = %d (%s), want 500", w.Code, w.Body.String())
	}
}

// TestRequireOrgScope_AllowsOrganizationsWriteWithoutAdmin proves org_owner
// (organizations:write, no admin wildcard) can manage its own organization —
// the entire point of the parity fix: org management no longer requires the
// flat platform-admin scope.
func TestRequireOrgScope_AllowsOrganizationsWriteWithoutAdmin(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["organizations:write"]`)
	e.mock.ExpectExec("DELETE FROM organization_members").WithArgs("org-a", "u2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodDelete, "/api/v1/admin/organizations/org-a/members/u2", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("organizations:write (no admin) removing a member of its own org: status = %d (%s), want 204", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected the org-scope lookup and the member removal to both run: %v", err)
	}
}

// TestRequireOrgScope_RejectsCrossOrgOrganizationsWrite proves the org-scoped
// non-admin path still respects the cross-org boundary: holding
// organizations:write in org-a must NOT authorize acting on org-b.
func TestRequireOrgScope_RejectsCrossOrgOrganizationsWrite(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	expectNoMembership(e.mock, "org-b", "caller-1")

	w := e.do(http.MethodDelete, "/api/v1/admin/organizations/org-b", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("caller with no membership in org-b: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DeleteOrganization must NOT be reached: %v", err)
	}
}

// ---------------------------------------------------------------------------
// requireSharedOrgAdminWithTargetUser
// ---------------------------------------------------------------------------

func TestRequireSharedOrgAdminWithTargetUser_AllowsWhenCallerAdminsSharedOrg(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	// Target user "target-1" belongs only to org-a.
	expectGetUserMemberships(e.mock, "target-1", "org-a")
	// Caller holds admin scope in org-a — a genuine shared-org relationship.
	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["admin"]`)
	// The handler itself (GetUserMemberships) re-queries the target's memberships.
	expectGetUserMemberships(e.mock, "target-1", "org-a")

	w := e.do(http.MethodGet, "/api/v1/admin/users/target-1/memberships", "")
	if w.Code != http.StatusOK {
		t.Fatalf("caller admins a shared org with the target user: status = %d (%s), want 200", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected the membership check then the handler's own query to run: %v", err)
	}
}

func TestRequireSharedOrgAdminWithTargetUser_RejectsWhenCallerNotAdminInAnyTargetOrg(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	// Target user belongs only to org-b, where the caller has no membership at all.
	expectGetUserMemberships(e.mock, "target-1", "org-b")
	expectNoMembership(e.mock, "org-b", "caller-1")

	w := e.do(http.MethodDelete, "/api/v1/admin/users/target-1", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("caller shares no admin org with the target user: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DeleteUser must NOT be reached: %v", err)
	}
}

func TestRequireSharedOrgAdminWithTargetUser_AllowsWhenTargetHasNoMemberships(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	// Target user has zero organization memberships at all (e.g. orphaned /
	// pre-provisioned) — nothing cross-tenant to protect, so this must pass
	// through without ever calling GetUserScopesForOrg.
	expectGetUserMemberships(e.mock, "target-1")
	expectGetUserMemberships(e.mock, "target-1") // the handler's own re-query

	w := e.do(http.MethodGet, "/api/v1/admin/users/target-1/memberships", "")
	if w.Code != http.StatusOK {
		t.Fatalf("target user with no memberships: status = %d (%s), want 200", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no GetUserScopesForOrg call expected when the target has no memberships: %v", err)
	}
}

func TestRequireSharedOrgAdminWithTargetUser_ChecksAllMembershipsUntilMatch(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	// Target user belongs to org-b (caller has no admin there) AND org-c (caller
	// IS admin there) — the middleware must not stop at the first non-matching
	// membership.
	expectGetUserMemberships(e.mock, "target-1", "org-b", "org-c")
	expectNoMembership(e.mock, "org-b", "caller-1")
	expectGetUserScopesForOrg(e.mock, "org-c", "caller-1", `["admin"]`)
	expectGetUserMemberships(e.mock, "target-1", "org-b", "org-c") // handler's own re-query

	w := e.do(http.MethodGet, "/api/v1/admin/users/target-1/memberships", "")
	if w.Code != http.StatusOK {
		t.Fatalf("caller admins the target's SECOND org membership: status = %d (%s), want 200", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected both memberships to be checked in order: %v", err)
	}
}

func TestRequireSharedOrgAdminWithTargetUser_MembershipsLookupDBError(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	e.mock.ExpectQuery("FROM organization_members om").WithArgs("target-1").
		WillReturnError(errors.New("db down"))

	w := e.do(http.MethodGet, "/api/v1/admin/users/target-1/memberships", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("target-memberships DB error: status = %d (%s), want 500", w.Code, w.Body.String())
	}
}

func TestRequireSharedOrgAdminWithTargetUser_ScopeLookupDBError(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	expectGetUserMemberships(e.mock, "target-1", "org-a")
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("org-a", "caller-1").
		WillReturnError(errors.New("db down"))

	w := e.do(http.MethodGet, "/api/v1/admin/users/target-1/memberships", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("caller-scope DB error: status = %d (%s), want 500", w.Code, w.Body.String())
	}
}

func TestRequireSharedOrgAdminWithTargetUser_MissingCallerIdentityIsForbidden(t *testing.T) {
	// No user_id in context at all — simulates a misconfigured chain rather than
	// a normal request (requireAuth always sets it), but must fail closed.
	e := newAdminOrgScopeEnv(t, "")

	w := e.do(http.MethodGet, "/api/v1/admin/users/target-1/memberships", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing caller identity: status = %d (%s), want 403", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no DB query should run without a caller id: %v", err)
	}
}

func TestRequireSharedOrgAdminWithTargetUser_DoesNotGateUserListOrCreate(t *testing.T) {
	// /admin/users (list/create) names no specific target user, so it must stay
	// gated only by the outer /admin ScopeAdmin check (exercised by the router's
	// own middleware chain in production, not this handler-level rig) —
	// requireSharedOrgAdminWithTargetUser must not be on its path at all. ListUsers
	// does derive the caller's admin orgs to narrow the list (#182), but that is a
	// result filter, not a gate: an empty list still returns 200.
	e := newAdminOrgScopeEnv(t, "caller-1")

	e.mock.ExpectQuery("FROM organization_members om").WithArgs("caller-1").
		WillReturnRows(sqlmock.NewRows(membershipCols))
	e.mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	e.mock.ExpectQuery("FROM users").WillReturnRows(sqlmock.NewRows(
		[]string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}))
	w := e.do(http.MethodGet, "/api/v1/admin/users", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list users: status = %d (%s), want 200 (not gated by the shared-org check)", w.Code, w.Body.String())
	}
}

// TestUsersInAdminOrgs directly exercises the #182 user-list narrowing: a user
// is kept only if they share an org with the caller's admin orgs, except
// membership-less users (no cross-tenant boundary) which are always kept.
func TestUsersInAdminOrgs(t *testing.T) {
	adminOrgs := map[string]struct{}{"org-a": {}}
	users := []*idmodels.UserWithOrgRoles{
		{User: idmodels.User{ID: "u-a"}, Memberships: []idmodels.UserMembership{{OrganizationID: "org-a"}}},
		{User: idmodels.User{ID: "u-b"}, Memberships: []idmodels.UserMembership{{OrganizationID: "org-b"}}},
		{User: idmodels.User{ID: "u-none"}, Memberships: nil},
	}
	got := usersInAdminOrgs(users, adminOrgs)
	kept := map[string]bool{}
	for _, u := range got {
		kept[u.ID] = true
	}
	if !kept["u-a"] || !kept["u-none"] {
		t.Errorf("expected u-a (shared org) and u-none (no memberships) kept, got %v", kept)
	}
	if kept["u-b"] {
		t.Errorf("u-b belongs only to a non-admin org and must be excluded, got %v", kept)
	}
}
