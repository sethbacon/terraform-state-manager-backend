package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
)

// #485 — the platform-admin carrier may stand in for membership on the bootstrap
// routes, and nowhere else.

type stubCarrier struct {
	isAdmin bool
	err     error
	asked   int
}

func (s *stubCarrier) IsPlatformAdmin(context.Context, string) (bool, error) {
	s.asked++
	return s.isAdmin, s.err
}

// bootstrapEnv mounts the org-admin routes with a carrier attached, and lets a
// test present either a session or an API key.
func bootstrapEnv(t *testing.T, carrier platformAdminSource, authMethod string, presented []string) *sourcesEnv {
	t.Helper()
	db, mock, err := newSQLMock()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := NewAdminHandlers(db, nil, approles.RoleSourceIdentity, WithPlatformAdmins(carrier))
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", "admin-1")
		c.Set("scopes", presented)
		c.Set("auth_method", authMethod)
		c.Next()
	})
	admin := r.Group("/api/v1/admin")
	admin.GET("/organizations", h.ListOrganizations())
	orgScoped := admin.Group("/organizations/:id", h.requireOrgScope())
	{
		orgScoped.DELETE("", h.DeleteOrganization())
		orgScoped.POST("/members", h.AddOrganizationMember())
	}
	return &sourcesEnv{r: r, mock: mock}
}

func do(env *sourcesEnv, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	env.r.ServeHTTP(w, req)
	return w
}

// A platform admin may ADD A MEMBER to an organization they do not belong to.
// Staging no GetUserScopesForOrg lookup is the assertion: reaching the
// membership check at all would fail against the unstaged query.
func TestRequireOrgScope_PlatformAdminMayBootstrapMembership(t *testing.T) {
	carrier := &stubCarrier{isAdmin: true}
	env := bootstrapEnv(t, carrier, "jwt", []string{"admin"})

	// The handler's own work, past the middleware.
	env.mock.ExpectQuery("FROM role_templates").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("rt-1"))
	env.mock.ExpectExec("INSERT INTO organization_members").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := do(env, http.MethodPost, "/api/v1/admin/organizations/org-other/members",
		`{"user_id":"u-1","role_template_id":"rt-1"}`)

	if w.Code == http.StatusForbidden {
		t.Fatalf("status = 403, want the bootstrap to be permitted: %s", w.Body.String())
	}
	if carrier.asked == 0 {
		t.Error("the carrier was never consulted, so the bypass cannot have been carrier-gated")
	}
}

// The bypass is route-scoped. DELETE of an organization is NOT a bootstrap step,
// so a platform admin who is not a member is still refused there.
func TestRequireOrgScope_PlatformAdminCannotDeleteAnOrgTheyAreNotIn(t *testing.T) {
	env := bootstrapEnv(t, &stubCarrier{isAdmin: true}, "jwt", []string{"admin"})
	expectNoMembership(env.mock, "org-other", "admin-1")

	w := do(env, http.MethodDelete, "/api/v1/admin/organizations/org-other", "")

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: deleting an organization is not a bootstrap step", w.Code)
	}
}

// An API key is refused even when its owner is a platform admin: a key is a
// narrowed credential and its owner's standing is not a statement about it.
func TestRequireOrgScope_ApiKeyNeverBootstraps(t *testing.T) {
	carrier := &stubCarrier{isAdmin: true}
	env := bootstrapEnv(t, carrier, "apikey", []string{"admin"})
	expectNoMembership(env.mock, "org-other", "admin-1")

	w := do(env, http.MethodPost, "/api/v1/admin/organizations/org-other/members",
		`{"user_id":"u-1","role_template_id":"rt-1"}`)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an API-key credential", w.Code)
	}
	if carrier.asked != 0 {
		t.Error("the carrier must not even be consulted for a key credential")
	}
}

// A carrier that cannot be reached answers "not a platform admin", so an outage
// narrows what a caller may do rather than widening it.
func TestRequireOrgScope_CarrierErrorDoesNotBootstrap(t *testing.T) {
	env := bootstrapEnv(t, &stubCarrier{err: errors.New("platform_admins unreadable")}, "jwt", []string{"admin"})
	expectNoMembership(env.mock, "org-other", "admin-1")

	w := do(env, http.MethodPost, "/api/v1/admin/organizations/org-other/members",
		`{"user_id":"u-1","role_template_id":"rt-1"}`)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when platform-admin standing cannot be established", w.Code)
	}
}

// The organization DIRECTORY is the other half: without it the organization a
// platform admin needs to administer appears nowhere they can select it.
func TestListOrganizations_PlatformAdminSeesEveryOrganization(t *testing.T) {
	carrier := &stubCarrier{isAdmin: true}
	env := bootstrapEnv(t, carrier, "jwt", []string{"admin"})

	// No membership lookup is staged: a platform admin's directory is not
	// derived from memberships at all.
	env.mock.ExpectQuery("FROM organizations").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "created_at", "updated_at"}))

	w := do(env, http.MethodGet, "/api/v1/admin/organizations", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if carrier.asked == 0 {
		t.Error("the carrier was never consulted")
	}
}

// The falsification, and the one that keeps the widening honest: an ordinary
// admin -- who holds the SAME flat `admin` scope, because in TSM admin is
// granted per organization and merely surfaces flat -- must still see only
// their own organizations.
func TestListOrganizations_OrdinaryAdminStillScopedToTheirOwn(t *testing.T) {
	carrier := &stubCarrier{isAdmin: false}
	env := bootstrapEnv(t, carrier, "jwt", []string{"admin"})

	// The membership-derived scope IS resolved for this caller. It comes back
	// empty, which scopes the listing to nothing -- the correct answer for an
	// admin whose memberships were not staged, and the opposite of the
	// platform-admin case above.
	env.mock.ExpectQuery("FROM organization_members").
		WillReturnRows(sqlmock.NewRows(scopeMemberCols))

	w := do(env, http.MethodGet, "/api/v1/admin/organizations", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	// The membership lookup having been CONSUMED is the assertion: it proves the
	// caller was scoped by membership rather than widened.
	if err := env.mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the membership lookup must still run for a non-carrier admin: %v", err)
	}
}
