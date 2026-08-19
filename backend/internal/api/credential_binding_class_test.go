// credential_binding_class_test.go guards the class #339 named: an authority
// ceiling derived from the OWNING USER instead of from the CREDENTIAL PRESENTING
// THE REQUEST.
//
// requireAuth is middleware.AuthMiddleware, which authenticates API keys as well
// as session JWTs. The API-key path narrows deliberately and twice — grantedSubset
// caps a key's stored scopes by its owner's live set, and idplatformadmin.KeyScopes
// strips `admin` unconditionally so an unattended CI credential cannot inherit its
// owner's platform-admin. Any handler behind requireAuth that then re-derives
// authority from the user id routes AROUND both narrowings: the key says
// state:read, the user record says admin, and only the user record is consulted.
//
// Three sites had it (all fixed in #339, one breaking change announced as one):
//
//  1. RefreshHandler minted a session JWT from GetUserCombinedScopes, so a
//     narrowed CI key was handed a cookie carrying its owner's whole cross-org
//     union — `admin` included, straight past the strip.
//  2. requireOrgScope was the SOLE authority decision on /admin/organizations/:id
//     and its member routes (the organizations:read/:create gates sit on the
//     collection routes, not the group), and it read the caller's role rows. A key
//     narrowed to state:read owned by an org_owner could delete the organization.
//  3. RotateAPIKey minted a replacement carrying the TARGET key's scopes without
//     re-validating them against the caller, so a narrow key could rotate a
//     broader sibling of the same owner and receive its new secret.
//
// The behavioural tests below assert BOTH DIRECTIONS on every one. A ceiling
// expressed as a set intersection over a scope lattice that has a wildcard
// (`admin`) and write-implies-read pairs can easily deny everyone — literally
// intersecting ["admin"] with ["state:read"] is empty — and a denial-only table
// cannot see that failure. So each fix is tested for what it still ALLOWS as well
// as for what it now refuses.
//
// The three structural guards after them are what catches the NEXT instance:
// they read the source tree, not a list somebody maintained by hand.
package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// 1. /auth/refresh: only an interactive session may be refreshed.
// ---------------------------------------------------------------------------

// TestRefresh_MachineCredentialsRefused covers every non-session auth_method the
// middleware can publish, plus the absent one.
//
// A 403 is itself the proof that no lookup happened: the credential check runs
// before GetUserByID, and a handler that reached the (unprimed) sqlmock would
// answer 401 "User not found" instead.
func TestRefresh_MachineCredentialsRefused(t *testing.T) {
	for _, tc := range []struct{ name, method string }{
		{"api key", "apikey"},
		{"mTLS client certificate", "mtls"},
		{"absent auth_method (mis-wired route)", ""},
		{"an auth method invented later", "webauthn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAuthEnvAs(t, "u1", tc.method, nil)
			w := e.do(http.MethodPost, "/api/v1/auth/refresh", "")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403.\n"+
					"A %q principal was allowed to exchange itself for a session JWT. That JWT is "+
					"minted from the OWNER's cross-organization scope union, so it carries authority "+
					"the presenting credential never held — including the `admin` that "+
					"idplatformadmin.KeyScopes strips from every API-key request (#339). Body: %s",
					w.Code, tc.method, w.Body.String())
			}
			for _, ck := range w.Result().Cookies() {
				if ck.Name == "tsm_auth_token" && ck.Value != "" {
					t.Errorf("a refused refresh still set a session cookie (%s)", ck.Name)
				}
			}
		})
	}
}

// TestRefresh_InteractiveSessionStillAllowed is the other direction, and it is
// not optional: a fix that refused everyone would pass every assertion above
// while breaking the only caller refresh exists for.
func TestRefresh_InteractiveSessionStillAllowed(t *testing.T) {
	for _, method := range []string{"jwt_cookie", "jwt"} {
		t.Run(method, func(t *testing.T) {
			e := newAuthEnvAs(t, "u1", method, nil)
			now := time.Now()
			e.mock.ExpectQuery("SELECT id, email, name, oidc_sub").WithArgs("u1").
				WillReturnRows(sqlmock.NewRows(idUserCols).AddRow("u1", "a@b.c", "Alice", "sub-1", now, now))
			e.mock.ExpectQuery("FROM organization_members om").WithArgs("u1").
				WillReturnRows(sqlmock.NewRows(membershipCols).
					AddRow("o1", "default", nil, now, "viewer", "Viewer", []byte(`["state:read"]`)))

			w := e.do(http.MethodPost, "/api/v1/auth/refresh", "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: a %s session must still refresh (%s)",
					w.Code, method, w.Body.String())
			}
			var gotAuth bool
			for _, ck := range w.Result().Cookies() {
				if ck.Name == "tsm_auth_token" && ck.HttpOnly && ck.Value != "" {
					gotAuth = true
				}
			}
			if !gotAuth {
				t.Error("no session cookie: refresh refused an interactive session")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. /admin/organizations/:id: the presenting credential is checked too.
// ---------------------------------------------------------------------------

// orgScopedRoutes is every route under the orgScoped group. All six are listed
// because #339's sibling was one axis of a group missing what another had, and a
// per-axis table is the only shape that sees that.
var orgScopedRoutes = []struct{ method, path string }{
	{http.MethodPut, "/api/v1/admin/organizations/org-a"},
	{http.MethodDelete, "/api/v1/admin/organizations/org-a"},
	{http.MethodGet, "/api/v1/admin/organizations/org-a/members"},
	{http.MethodPost, "/api/v1/admin/organizations/org-a/members"},
	{http.MethodPut, "/api/v1/admin/organizations/org-a/members/u2"},
	{http.MethodDelete, "/api/v1/admin/organizations/org-a/members/u2"},
}

// TestRequireOrgScope_NarrowCredentialOfOrgOwnerRefused presents the exact
// escalation: a key capped at state:read whose OWNER is an org_owner of the
// target organization (org_owner carries organizations:write). The per-org
// re-derivation would say yes; the credential must say no first.
//
// No membership lookup is primed, so reaching one would fail the request for the
// wrong reason — the 403 has to come from the credential check ahead of it.
func TestRequireOrgScope_NarrowCredentialOfOrgOwnerRefused(t *testing.T) {
	for _, rt := range orgScopedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			e := newAdminOrgScopeEnvAs(t, "caller-1", []string{"state:read", "state:drift"})
			w := e.do(rt.method, rt.path, `{"name":"pwned"}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403.\n"+
					"A credential carrying only state:read reached an organization-management "+
					"route. requireOrgScope re-derives the caller's scopes from their ROLE ROWS, "+
					"and the orgScoped group has no other gate — so the owner's org_owner role "+
					"authorized a request the credential never could (#339). Body: %s",
					w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "within the target organization") {
				t.Error("the refusal came from the per-organization check, not the credential " +
					"check: the credential check must run FIRST, before any membership lookup")
			}
		})
	}
}

// TestRequireOrgScope_OrgOwnerSessionStillAllowed is the direction that catches a
// ceiling which denies everyone. org_owner holds organizations:write and never
// the admin wildcard, so this is the narrowest principal the routes must serve.
func TestRequireOrgScope_OrgOwnerSessionStillAllowed(t *testing.T) {
	e := newAdminOrgScopeEnvAs(t, "caller-1", []string{"organizations:write", "users:read"})
	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["organizations:write","users:read"]`)
	e.mock.ExpectQuery("FROM organization_members om").
		WillReturnRows(sqlmock.NewRows(scopeMemberCols))

	w := e.do(http.MethodGet, "/api/v1/admin/organizations/org-a/members", "")
	if w.Code == http.StatusForbidden {
		t.Fatalf("an org_owner session was refused its own organization (%s).\n"+
			"organizations:write must satisfy the credential check — a ceiling that only "+
			"admin can clear locks every org_owner out of the routes built for them",
			w.Body.String())
	}
}

// TestRequireOrgScope_PlatformAdminSessionStillAllowed pins the wildcard leg.
// `admin` implies every scope through auth.HasScope, and a ceiling written as a
// literal set intersection would drop it and lock out every platform admin.
func TestRequireOrgScope_PlatformAdminSessionStillAllowed(t *testing.T) {
	e := newAdminOrgScopeEnvAs(t, "caller-1", []string{"admin"})
	expectGetUserScopesForOrg(e.mock, "org-a", "caller-1", `["admin"]`)
	e.mock.ExpectQuery("FROM organization_members om").
		WillReturnRows(sqlmock.NewRows(scopeMemberCols))

	w := e.do(http.MethodGet, "/api/v1/admin/organizations/org-a/members", "")
	if w.Code == http.StatusForbidden {
		t.Fatalf("a caller holding the `admin` wildcard was refused (%s).\n"+
			"HasScope treats admin as satisfying every scope; a ceiling that compares "+
			"scope strings directly loses that and denies the wildcard it is meant to honour",
			w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 3. /apikeys/:id/rotate: the mint ceiling is re-validated against the caller.
// ---------------------------------------------------------------------------

// TestRotate_NarrowKeyCannotRotateBroaderSibling is the escalation: caller holds
// state:read (an API key's capped set); the target key of the SAME OWNER holds
// state:write and sources:manage. ownsOrAdmin admits it on ownership alone, so
// without the re-validation the response hands over a fresh secret for the
// broader key.
func TestRotate_NarrowKeyCannotRotateBroaderSibling(t *testing.T) {
	e := newAPIKeysEnv(t)
	*e.scopes = []string{"state:read"}
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k2").
		WillReturnRows(apiKeyDBRow("k2", "u1", `["state:write","sources:manage"]`))

	w := e.do(http.MethodPost, "/api/v1/apikeys/k2/rotate", `{"grace_period_hours":0}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403.\n"+
			"A credential holding only state:read rotated a key holding state:write and "+
			"sources:manage, and the new plaintext secret is in this response. Rotation's "+
			"mint ceiling came from the TARGET KEY's row, checked against the OWNER's "+
			"identity rather than against the caller's scopes (#339). Body: %s",
			w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tsm_") {
		t.Error("a refused rotation still returned a plaintext key")
	}
}

// TestRotate_SelfRotationStillAllowed is the direction that matters most for a
// machine credential: a key must always be able to rotate ITSELF, or unattended
// rotation stops working the moment this guard lands.
func TestRotate_SelfRotationStillAllowed(t *testing.T) {
	e := newAPIKeysEnv(t)
	*e.scopes = []string{"state:read", "state:drift"}
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k1").
		WillReturnRows(apiKeyDBRow("k1", "u1", `["state:read","state:drift"]`))
	expectDefaultOrg(e.mock)
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM api_keys").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/apikeys/k1/rotate", `{"grace_period_hours":0}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: a key must still rotate itself (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"key":"tsm_`) {
		t.Errorf("self-rotation must still return the new secret exactly once: %s", w.Body.String())
	}
}

// TestRotate_AdminStillRotatesAnyKey pins the wildcard leg here too: `admin`
// satisfies HasScope for every scope, so an admin must keep being able to rotate
// a key whose scopes they do not literally list.
func TestRotate_AdminStillRotatesAnyKey(t *testing.T) {
	e := newAPIKeysEnv(t)
	*e.scopes = []string{"admin"}
	e.mock.ExpectQuery("FROM api_keys").WithArgs("k9").
		WillReturnRows(apiKeyDBRow("k9", "someone-else", `["state:write","sources:manage"]`))
	expectDefaultOrg(e.mock)
	e.mock.ExpectExec("INSERT INTO api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	e.mock.ExpectExec("DELETE FROM api_keys").WillReturnResult(sqlmock.NewResult(0, 1))

	w := e.do(http.MethodPost, "/api/v1/apikeys/k9/rotate", `{"grace_period_hours":0}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: `admin` must still rotate any key (%s)", w.Code, w.Body.String())
	}
}
