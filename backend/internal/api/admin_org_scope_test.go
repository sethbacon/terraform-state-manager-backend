package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
)

// newAdminOrgScopeEnv wires the /admin/organizations/:id* routes the same way
// router.go does: a stand-in for requireAuth sets user_id in the gin context
// (the real middleware chain populates this from the validated JWT), then
// requireOrgScope gates the :id-scoped routes ahead of the handler.
func newAdminOrgScopeEnv(t *testing.T, callerUserID string) *sourcesEnv {
	t.Helper()
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewAdminHandlers(db, nil, approles.RoleSourceIdentity)
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

// expectCallerAdminIn stubs the caller's OWN membership rows with an
// admin-granting role template, which is what OrgScopeForUser reads to resolve
// the caller's scope. expectGetUserMemberships hardcodes a "placeholder" scope
// that grants nothing, so it cannot be used for the caller side.
func expectCallerAdminIn(mock sqlmock.Sqlmock, callerID string, orgIDs ...string) {
	rows := sqlmock.NewRows(userMembershipCols)
	for _, orgID := range orgIDs {
		rows.AddRow(orgID, "Org "+orgID, "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`))
	}
	mock.ExpectQuery("FROM organization_members om").WithArgs(callerID).WillReturnRows(rows)
}

func TestRequireOrgScope_AllowsAdminActingOnOwnOrg(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["admin"]`)
	// The members list now also binds the route's own OrgScope (routeOrgScope),
	// which requireOrgScope has already proved the caller holds.
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("org-a", []string{"org-a"}).
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
	e.mock.ExpectExec("DELETE FROM organization_members").WithArgs("org-a", "u2", []string{"org-a"}).
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

	// The list still runs without requireOrgScope, but it now narrows itself to
	// the organizations the caller holds organizations:read in (identity #161),
	// so the caller's memberships are resolved first.
	e.mock.ExpectQuery("FROM organization_members om").WithArgs("caller-1").
		WillReturnRows(sqlmock.NewRows(userMembershipCols).
			AddRow("org-a", "Org A", "rt-owner", time.Now(), "org_owner", "Owner", []byte(`["organizations:write"]`)))
	e.mock.ExpectQuery("FROM organizations").WithArgs([]string{"org-a"}, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(
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
		WithArgs("org-1", "caller-1", "rt-org-owner", []string{"org-1"}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectQuery("INSERT INTO audit_logs").WillReturnRows(auditInsertReturn())

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
	e.mock.ExpectExec("DELETE FROM organization_members").WithArgs("org-a", "u2", []string{"org-a"}).
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
	// The handler resolves the CALLER's scope first (identity #183): it must
	// only return memberships in organizations the caller actually administers,
	// not every organization the target belongs to.
	expectCallerAdminIn(e.mock, "caller-1", "org-a")
	// Then it re-queries the target's memberships and filters them.
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
	// The handler resolves the caller's scope, then re-queries the target
	// (identity #183).
	expectCallerAdminIn(e.mock, "caller-1", "org-a")
	expectGetUserMemberships(e.mock, "target-1")

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
	// The handler resolves the caller's scope (org-c only), then re-queries the
	// target and filters org-b out of the response — the caller administers
	// nothing there (identity #183).
	expectCallerAdminIn(e.mock, "caller-1", "org-c")
	expectGetUserMemberships(e.mock, "target-1", "org-b", "org-c")

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

// wantUserMembershipPredicate is the SQL the store emits for "shares an
// organization with the caller, or belongs to none at all" — the #182 user-list
// narrowing.
//
// This replaces TestUsersInAdminOrgs, which exercised the in-memory
// usersInAdminOrgs post-filter deleted in the identity v0.25.0 bump. The RULE is
// unchanged and still has to hold; what changed is where it is enforced, and
// that is precisely why the assertion had to move. A post-filter is observable
// in the response; a predicate is observable ONLY in the statement, so a handler
// that dropped its scope would still answer 200 with whatever rows the mock was
// told to return. Asserting the literal fragment also separates the two ways
// this can break: losing the scope removes the clause entirely, while narrowing
// to OrgScopeOrganizations drops the NOT EXISTS half and hides every
// membership-less user.
const (
	wantUserInScope   = "EXISTS (SELECT 1 FROM organization_members osm WHERE osm.user_id = users.id AND osm.organization_id = ANY($1))"
	wantUserNoMembers = "NOT EXISTS (SELECT 1 FROM organization_members osm WHERE osm.user_id = users.id)"
)

// TestListUsers_NarrowsToCallerAdminOrgs pins the #182 narrowing as the query's
// own predicate: a user is visible only if they share an organization the caller
// administers, except membership-less users (no cross-tenant boundary to
// protect), who stay visible.
func TestListUsers_NarrowsToCallerAdminOrgs(t *testing.T) {
	rec := &auditSQLRecorder{}
	db, mock, err := newSQLMockMatching(
		sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
			rec.record(actualSQL)
			return sqlmock.QueryMatcherRegexp.Match(expectedSQL, actualSQL)
		}),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewAdminHandlers(db, nil, approles.RoleSourceIdentity)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", auditScopeCaller); c.Next() })
	r.GET("/users", h.ListUsers())

	// The caller administers org-a and is a plain viewer in org-c, so only org-a
	// may reach the predicate.
	mock.ExpectQuery("FROM organization_members om").WithArgs(auditScopeCaller).
		WillReturnRows(sqlmock.NewRows(userMembershipCols).
			AddRow(auditScopeOrgA, "Org A", "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`)).
			AddRow("org-c", "Org C", "rt-viewer", time.Now(), "viewer", "Viewer", []byte(`["state:read"]`)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM users`).WithArgs([]string{auditScopeOrgA}).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery("FROM users").
		WithArgs([]string{auditScopeOrgA}, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "name", "oidc_sub", "created_at", "updated_at"}).
			AddRow("u-a", "a@x.io", "A", nil, time.Now(), time.Now()).
			AddRow("u-none", "n@x.io", "N", nil, time.Now(), time.Now()))
	mock.ExpectQuery("FROM organization_members om").
		WithArgs([]string{"u-a", "u-none"}, []string{auditScopeOrgA}).
		WillReturnRows(sqlmock.NewRows(append([]string{"user_id"}, userMembershipCols...)).
			AddRow("u-a", auditScopeOrgA, "Org A", "rt-admin", time.Now(), "admin", "Admin", []byte(`["admin"]`)))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/users", nil))

	// The guard first, and independently of the response: a dropped scope makes
	// the bind arguments stop matching and surfaces as an uninformative 500.
	var userReads int
	for _, q := range rec.seen {
		if !strings.Contains(q, "FROM users") {
			continue
		}
		userReads++
		if !strings.Contains(q, wantUserInScope) {
			t.Errorf("user read is not tenant-scoped — it can enumerate another tenant's users.\nwant fragment: %s\ngot statement:  %s", wantUserInScope, q)
		}
		if !strings.Contains(q, wantUserNoMembers) {
			t.Errorf("user read dropped the membership-less axis — a user belonging to no organization becomes invisible to every admin.\nwant fragment: %s\ngot statement:  %s", wantUserNoMembers, q)
		}
	}
	if userReads == 0 {
		t.Fatal("no users read reached the database")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// Nothing may re-narrow after the database: rows the predicate admitted must
	// reach the response. This is what fails if a post-filter is reintroduced.
	body := w.Body.String()
	for _, id := range []string{"u-a", "u-none"} {
		if !strings.Contains(body, id) {
			t.Errorf("user %q was admitted by the scope but is missing from the response: %s", id, body)
		}
	}
	// ...and the count must be the scoped one the repository returned, not a
	// page-relative number a post-filter forced.
	if !strings.Contains(body, `"total":2`) {
		t.Errorf("total must be the repository's scoped count, got %s", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("queued round-trips did not all run: %v", err)
	}
}

// TestGetUserMemberships_OmitsOrgsTheCallerDoesNotAdminister is the disclosure
// itself (identity #183).
//
// requireSharedOrgAdminWithTargetUser proves the caller administers AT LEAST
// ONE organization the target belongs to. That authorises asking about the
// user; it does not authorise seeing every organization the user belongs to.
// Before this, an org-a admin received the target's membership rows for org-b
// and org-c — organizations the caller belongs to nowhere — including their
// names, role templates and role-template scopes.
//
// The shared module predicted exactly this: store.GetUserMemberships is
// documented "UNSCOPED BY DESIGN — authority derivation" and says the consumer
// must guard it when asking about SOMEONE ELSE.
func TestGetUserMemberships_OmitsOrgsTheCallerDoesNotAdminister(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	// The target belongs to three organizations; the caller administers only
	// org-a. The guard therefore admits the request.
	expectGetUserMemberships(e.mock, "target-1", "org-a", "org-b", "org-c")
	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["admin"]`)
	// Caller scope resolution: admin in org-a only.
	expectCallerAdminIn(e.mock, "caller-1", "org-a")
	// The target's rows, which the handler must now filter.
	expectGetUserMemberships(e.mock, "target-1", "org-a", "org-b", "org-c")

	w := e.do(http.MethodGet, "/api/v1/admin/users/target-1/memberships", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "org-a") {
		t.Errorf("the caller's own organization is missing from the response: %s", body)
	}
	for _, hidden := range []string{"org-b", "org-c"} {
		if strings.Contains(body, hidden) {
			t.Errorf("response discloses %s, an organization the caller does not "+
				"administer: %s", hidden, body)
		}
	}
}

// TestExportUserData_MembershipsAreScopedLikeTheAuditTrail — the GDPR export
// scoped its audit read (GUARD audit-scope-user-export, #331) with a comment
// giving exactly this reasoning, then shipped the target's memberships
// unfiltered in the same document.
func TestExportUserData_MembershipsAreScopedLikeTheAuditTrail(t *testing.T) {
	e := newAdminOrgScopeEnv(t, "caller-1")

	// Guard: the target is in org-a and org-b; the caller administers org-a.
	expectGetUserMemberships(e.mock, "target-1", "org-a", "org-b")
	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["admin"]`)
	// callerScopeFor for the export body.
	expectCallerAdminIn(e.mock, "caller-1", "org-a")
	e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("target-1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(idUserCols).
			AddRow("target-1", "t@example.com", "Target", nil, time.Now(), time.Now()))
	// The target's memberships, which must now be filtered to org-a.
	expectGetUserMemberships(e.mock, "target-1", "org-a", "org-b")
	e.mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	e.mock.ExpectQuery("FROM audit_logs al").WillReturnRows(sqlmock.NewRows(auditCols))

	w := e.do(http.MethodGet, "/api/v1/admin/users/target-1/export", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "org-a") {
		t.Errorf("export is missing the caller's own organization: %s", body)
	}
	if strings.Contains(body, "org-b") {
		t.Errorf("GDPR export discloses org-b, which the caller does not administer — "+
			"the audit trail beside it is scoped and the memberships were not: %s", body)
	}
}
