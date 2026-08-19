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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
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

// ---------------------------------------------------------------------------
// Structural guards. The three fixes above are instances; these are the class.
//
// Every guard here ENUMERATES from the source tree rather than checking a list
// somebody typed. A guard that asserts "these three handlers are fine" is worth
// nothing the moment a fourth is written, which is exactly how #339 arrived —
// the sibling in requireOrgScope had been sitting behind the same middleware as
// the reported instance the whole time.
// ---------------------------------------------------------------------------

// callersOfFunc returns "<path>:<enclosing top-level func>" for every CALL of
// name in the non-test tree, matching both a qualified call (x.Name(...)) and a
// package-local one (Name(...)).
//
// It matches only the CALLEE position of a call expression, so a doc comment
// (this file is full of them), a variable that happens to share the name, or the
// function's own declaration is never mistaken for a call site. Calls inside a
// closure are attributed to the top-level function that returns it, which is what
// every gin handler in this package is.
func callersOfFunc(t *testing.T, name string) []string {
	t.Helper()
	var found []string
	fset := token.NewFileSet()
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(path), "../../"))
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			hit := false
			ast.Inspect(fd, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					hit = hit || fun.Name == name
				case *ast.SelectorExpr:
					hit = hit || fun.Sel.Name == name
				}
				return true
			})
			if hit {
				found = append(found, rel+":"+fd.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan backend tree: %v", err)
	}
	return found
}

// sessionMintingCall is the one function that turns a scope set into a session
// JWT. Everything downstream of it — the cookie, the TTL, the JTI — inherits
// whatever authority was handed to it, so it is the choke point the class is
// defined at.
const sessionMintingCall = "GenerateJWT"

// credentialGateCall is the predicate a session minter behind requireAuth must
// apply: it reads auth_method, which only the middleware writes, so it cannot be
// satisfied by anything the request controls.
const credentialGateCall = "isInteractiveSession"

// unauthenticatedSessionMinters are the session-minting functions that legitimately
// derive authority from the user record, because they run BEFORE any credential
// exists — they are the login flows that create the first one. The value is the
// route each is mounted on, and TestUnauthenticatedSessionMintersAreReallyUnauthenticated
// proves against router.go that the route really is outside requireAuth.
//
// This is the recorded adjudication of #339's four flagged login handlers. The
// signature that raised #339 flagged all four alongside RefreshHandler; they are
// FALSE POSITIVES, because a handler that has no presenting credential cannot
// bind to one. That is a claim about ROUTING, not about the handler bodies — so
// it is checked against the router rather than asserted in prose, and the day one
// of these is moved behind requireAuth the exemption's premise dies and this
// file says so.
var unauthenticatedSessionMinters = map[string]string{
	"internal/api/auth.go:CallbackHandler":        "/api/v1/auth/callback",
	"internal/api/ldap_login.go:LDAPLoginHandler": "/api/v1/auth/ldap/login",
	"internal/api/saml_login.go:SAMLACSHandler":   "/api/v1/auth/saml/acs",
	"internal/api/dev.go:DevLoginHandler":         "/api/v1/dev/login",
}

// TestSessionMintersAreCredentialBound is the class guard for #339.
//
// A session JWT carries authority. Minting one from the OWNER's role rows on a
// path an API key can reach is the defect; the only two safe shapes are "there
// is no credential yet" (a login flow, listed above and proved unauthenticated
// by routing) and "the credential was checked first" (isInteractiveSession).
// Any third shape fails here, with no way to make it pass except deleting the
// derivation, adding the gate, or writing the handler down as a login flow — and
// that last one is checked against the router, not taken on trust.
func TestSessionMintersAreCredentialBound(t *testing.T) {
	minters := callersOfFunc(t, sessionMintingCall)
	if len(minters) == 0 {
		t.Fatalf("no function calls %s. Either sessions are no longer minted, or the "+
			"scanner is not reading the source tree — in which case this guard and every "+
			"other one in this file are silently passing.", sessionMintingCall)
	}

	gated := map[string]bool{}
	for _, site := range callersOfFunc(t, credentialGateCall) {
		gated[site] = true
	}

	for _, site := range minters {
		if _, ok := unauthenticatedSessionMinters[site]; ok {
			continue
		}
		if !gated[site] {
			t.Errorf("%s mints a session JWT but never calls %s.\n"+
				"Behind requireAuth the caller may be an API key, and a session minted from the "+
				"OWNER's scopes carries authority that key never held — grantedSubset's cap and "+
				"KeyScopes' unconditional `admin` strip are both bypassed (#339).\n"+
				"Either gate it on the presenting credential, or — if it is a login flow with no "+
				"credential to bind to — add it to unauthenticatedSessionMinters with its route, "+
				"which is then verified against router.go.",
				site, credentialGateCall)
		}
	}

	for site := range unauthenticatedSessionMinters {
		found := false
		for _, m := range minters {
			found = found || m == site
		}
		if !found {
			t.Errorf("unauthenticatedSessionMinters lists %s, which no longer mints a session. "+
				"Remove the entry so the list keeps meaning what it says.", site)
		}
	}
}

// keyMintingCall persists a new API key and returns its plaintext secret.
const keyMintingCall = "mintKey"

// keyCeilingCall bounds a requested scope set by the CALLER's own scopes.
const keyCeilingCall = "validateGrantedScopes"

// TestKeyMintersValidateTheCeiling is the same class one credential kind over.
// An API key is authority too, and rotation is a minting path that takes its
// scopes from a stored row rather than from the request body — which is exactly
// how it came to skip the check its create and update siblings both had.
func TestKeyMintersValidateTheCeiling(t *testing.T) {
	minters := callersOfFunc(t, keyMintingCall)
	if len(minters) == 0 {
		t.Fatalf("no function calls %s — the scanner is not seeing the key-minting path "+
			"it is meant to guard.", keyMintingCall)
	}
	bounded := map[string]bool{}
	for _, site := range callersOfFunc(t, keyCeilingCall) {
		bounded[site] = true
	}
	for _, site := range minters {
		if !bounded[site] {
			t.Errorf("%s mints an API key but never calls %s.\n"+
				"The scopes it stamps on the new key must be bounded by the scopes of the "+
				"credential asking for it, or a narrow key mints a broad one (#339).",
				site, keyCeilingCall)
		}
	}
}

// ---------------------------------------------------------------------------
// The route-shape enumerator.
//
// #339's sibling was not a handler that did something exotic; it was a ROUTE
// GROUP whose only gates were requireAuth and a user-derived check. Nothing at
// the handler level distinguishes that from a correctly gated group — the
// difference is in the middleware chain — so the guard has to read the chain.
// ---------------------------------------------------------------------------

// routeSpec is one registered route with its FULL middleware chain, group
// nesting flattened.
type routeSpec struct {
	method  string
	path    string
	handler string
	chain   []string
}

func (r routeSpec) key() string { return r.method + " " + r.path }

func (r routeSpec) has(mw string) bool {
	for _, m := range r.chain {
		if m == mw {
			return true
		}
	}
	return false
}

// scopeGateCalls are the middlewares that decide on the scopes the auth
// middleware published for THIS request. requireOrgScope is deliberately NOT
// here: it re-derives from the user id, and treating it as a gate is precisely
// the reading that let #339's sibling sit behind requireAuth alone.
var scopeGateCalls = []string{"RequireScope", "RequireAnyScope", "RequireAllScopes"}

func (r routeSpec) hasScopeGate() bool {
	for _, g := range scopeGateCalls {
		if r.has(g) {
			return true
		}
	}
	return false
}

// parseRouterRoutes reads internal/api/router.go and reconstructs every route
// with the middleware chain it actually inherits.
//
// It follows `x := y.Group(path, mw...)` assignments, `x.Use(mw...)` calls and
// `x.VERB(path, mw..., handler)` registrations, so a route's chain is its own
// middlewares plus every enclosing group's, in nesting order. Middlewares are
// named by their callee (middleware.RequireScope(...) -> "RequireScope",
// admin.requireOrgScope() -> "requireOrgScope", the bare ident requireAuth ->
// "requireAuth"), which is enough to ask the only question that matters here:
// what decided this request was allowed.
func parseRouterRoutes(t *testing.T) []routeSpec {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("router.go"), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse router.go: %v", err)
	}

	type group struct {
		path   string
		chain  []string
		parent string
	}
	groups := map[string]*group{}
	// A receiver never assigned from .Group (the engine itself) is a root.
	groupOf := func(name string) *group {
		if g, ok := groups[name]; ok {
			return g
		}
		g := &group{}
		groups[name] = g
		return g
	}
	// resolve walks a group's ancestry, returning the full path and chain.
	var resolve func(name string) (string, []string)
	resolve = func(name string) (string, []string) {
		g := groupOf(name)
		if g.parent == "" {
			return g.path, append([]string{}, g.chain...)
		}
		ppath, pchain := resolve(g.parent)
		return ppath + g.path, append(pchain, g.chain...)
	}

	// exprName renders a middleware or handler expression to its callee name.
	var exprName func(ast.Expr) string
	exprName = func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.Ident:
			return v.Name
		case *ast.SelectorExpr:
			return v.Sel.Name
		case *ast.CallExpr:
			return exprName(v.Fun)
		}
		return ""
	}
	strLit := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		return strings.Trim(lit.Value, `"`), true
	}
	verbs := map[string]bool{
		"GET": true, "POST": true, "PUT": true, "PATCH": true,
		"DELETE": true, "HEAD": true, "OPTIONS": true,
	}

	var routes []routeSpec
	// Source order matters: a group is always defined before it is used.
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != 1 || len(node.Rhs) != 1 {
				return true
			}
			lhs, ok := node.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			call, ok := node.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Group" || len(call.Args) == 0 {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			path, _ := strLit(call.Args[0])
			g := &group{path: path, parent: recv.Name}
			for _, a := range call.Args[1:] {
				g.chain = append(g.chain, exprName(a))
			}
			groups[lhs.Name] = g
		case *ast.ExprStmt:
			call, ok := node.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Use" {
				g := groupOf(recv.Name)
				for _, a := range call.Args {
					g.chain = append(g.chain, exprName(a))
				}
				return true
			}
			if !verbs[sel.Sel.Name] || len(call.Args) < 2 {
				return true
			}
			rpath, ok := strLit(call.Args[0])
			if !ok {
				return true
			}
			gpath, gchain := resolve(recv.Name)
			r := routeSpec{method: sel.Sel.Name, path: gpath + rpath, chain: gchain}
			// Everything between the path and the final handler is middleware.
			for _, a := range call.Args[1 : len(call.Args)-1] {
				r.chain = append(r.chain, exprName(a))
			}
			r.handler = exprName(call.Args[len(call.Args)-1])
			routes = append(routes, r)
		}
		return true
	})
	return routes
}

// TestRouteParserSeesTheRouter is the non-vacuity check for the parser. Every
// guard below reports "clean" by finding nothing wrong, so a parser that walked
// the wrong file, stopped following .Group, or lost the verbs would make all of
// them pass while checking nothing at all.
func TestRouteParserSeesTheRouter(t *testing.T) {
	routes := parseRouterRoutes(t)
	if len(routes) < 50 {
		t.Fatalf("parsed only %d routes from router.go; the real router has ~100. "+
			"The parser has lost the tree, and every guard built on it is now vacuous.", len(routes))
	}
	var authed, gated int
	for _, r := range routes {
		if r.has("requireAuth") {
			authed++
			if r.hasScopeGate() {
				gated++
			}
		}
	}
	if authed == 0 {
		t.Fatal("no route parsed as being behind requireAuth: the middleware chain is not " +
			"being resolved, so the credential-binding guards see an empty universe")
	}
	if gated == 0 {
		t.Fatal("no route parsed as carrying a scope gate: middleware.RequireScope is no " +
			"longer being recognised, so every route looks ungated and the reviewed list " +
			"below would be accepted as complete when it is not")
	}
	// Anchors: the two shapes #339 turns on must both be visible.
	var sawRefresh, sawCallback bool
	for _, r := range routes {
		if r.key() == "POST /api/v1/auth/refresh" {
			sawRefresh = true
			if !r.has("requireAuth") {
				t.Error("POST /auth/refresh is no longer behind requireAuth")
			}
		}
		if r.key() == "GET /api/v1/auth/callback" {
			sawCallback = true
		}
	}
	if !sawRefresh || !sawCallback {
		t.Fatalf("anchor routes missing (refresh=%v callback=%v): the parser is not "+
			"resolving the /auth group", sawRefresh, sawCallback)
	}
}

// TestUnauthenticatedSessionMintersAreReallyUnauthenticated verifies #339's
// recorded false positives against the router instead of against prose.
//
// The four login handlers are exempt from TestSessionMintersAreCredentialBound
// on one claim only: they are mounted outside requireAuth, so no credential has
// been presented and there is nothing to bind to. That claim is checked here.
// Move any of them behind requireAuth and the exemption becomes an unguarded
// session minter, which is #339 again.
func TestUnauthenticatedSessionMintersAreReallyUnauthenticated(t *testing.T) {
	routes := parseRouterRoutes(t)
	byHandler := map[string][]routeSpec{}
	for _, r := range routes {
		byHandler[r.handler] = append(byHandler[r.handler], r)
	}
	for site, wantPath := range unauthenticatedSessionMinters {
		fn := site[strings.LastIndex(site, ":")+1:]
		mounted := byHandler[fn]
		if len(mounted) == 0 {
			t.Errorf("%s is exempted as an unauthenticated login flow, but router.go mounts "+
				"no route on it. An exemption for a handler that is not routed proves nothing; "+
				"remove it, or point it at the route that exists.", site)
			continue
		}
		for _, r := range mounted {
			if r.path != wantPath {
				t.Errorf("%s is recorded at %s but is mounted at %s. Update the entry so the "+
					"claim being checked is the one being made.", site, wantPath, r.path)
			}
			if r.has("requireAuth") {
				t.Errorf("%s is exempted from the credential-binding guard because it is an "+
					"UNAUTHENTICATED login flow, but %s is now behind requireAuth.\n"+
					"An API key can reach it, so it mints a session from the owner's scopes on a "+
					"path that has a presenting credential — #339 exactly. Either gate it on %s, "+
					"or take it back out of requireAuth.", site, r.key(), credentialGateCall)
			}
		}
	}
}

// reviewedUngatedRoutes are the routes that sit behind requireAuth with NO
// middleware.RequireScope gate, each mapped to how its handler nonetheless binds
// authority to the credential presenting the request.
//
// The bar for an entry: name the mechanism, and make it one that reads the
// REQUEST's established scopes (scopesOf / c.Get("scopes")) or its auth_method.
// "The handler checks ownership" is not sufficient on its own for anything that
// MINTS or WIDENS — ownership is a fact about the user record, and #339 is the
// class of ceilings read off the user record. Ownership is sufficient for reads
// and for revocation, which cannot hand out authority.
//
// A route added behind requireAuth without a scope gate and without an entry
// here fails the build. That is the point: #339's sibling was a whole group
// (/admin/organizations/:id) whose only gates were requireAuth and a user-derived
// check, and nothing in the tree objected for as long as it stood.
var reviewedUngatedRoutes = map[string]string{
	"GET /api/v1/auth/me": "reports, never mints: reportedScopes reconciles the `admin` bit against " +
		"this request's EFFECTIVE scopes, so an API key is never told it holds authority the " +
		"middleware stripped from it",
	"POST /api/v1/auth/refresh": "gated on isInteractiveSession (#339): only a session JWT may be " +
		"exchanged for a session JWT, so the owner's scope union is never read for a machine credential",

	"GET /api/v1/apikeys": "read-only listing; the non-admin branch is ListAPIKeysByUser (own keys), " +
		"and the admin branch is narrowed by OrgScopeForUser to keys whose owner shares an " +
		"organization the caller administers (#182). No authority is conferred",
	"POST /api/v1/apikeys": "validateGrantedScopes bounds the requested scopes by scopesOf(c) — the " +
		"REQUEST's effective scopes — so a key can only ever mint a narrower key",
	"GET /api/v1/apikeys/:id":    "read-only, ownsOrAdmin; never returns the secret",
	"PUT /api/v1/apikeys/:id":    "ownsOrAdmin plus validateGrantedScopes against scopesOf(c), so an update cannot widen a key past the caller",
	"DELETE /api/v1/apikeys/:id": "revocation only, ownsOrAdmin; removing authority cannot confer any",
	"POST /api/v1/apikeys/:id/rotate": "ownsOrAdmin plus validateGrantedScopes against the TARGET KEY's " +
		"scopes (#339), so a narrow key cannot rotate a broader sibling of the same owner and be handed its secret",

	"PUT /api/v1/admin/organizations/:id": "requireOrgScope checks the PRESENTING credential for " +
		"organizations:write/admin before re-deriving per-organization scopes (#339); no API key can carry either",
	"DELETE /api/v1/admin/organizations/:id":                  "as PUT /admin/organizations/:id",
	"GET /api/v1/admin/organizations/:id/members":             "as PUT /admin/organizations/:id",
	"POST /api/v1/admin/organizations/:id/members":            "as PUT /admin/organizations/:id",
	"PUT /api/v1/admin/organizations/:id/members/:user_id":    "as PUT /admin/organizations/:id",
	"DELETE /api/v1/admin/organizations/:id/members/:user_id": "as PUT /admin/organizations/:id",
}

// TestRoutesBehindRequireAuthBindAuthority enumerates the routes an API key can
// reach and forces every one of them to have decided, in writing, how it bounds
// authority.
//
// requireAuth is NOT an authorization decision — it establishes who is calling
// and with what. A route that stops there is relying entirely on its handler, and
// the handler is where #339 lived three times over.
func TestRoutesBehindRequireAuthBindAuthority(t *testing.T) {
	routes := parseRouterRoutes(t)
	seen := map[string]bool{}
	for _, r := range routes {
		if !r.has("requireAuth") || r.hasScopeGate() {
			continue
		}
		seen[r.key()] = true
		if _, ok := reviewedUngatedRoutes[r.key()]; !ok {
			t.Errorf("%s is behind requireAuth with no scope gate (chain: %s).\n"+
				"An API key reaches it, and requireAuth only says WHO is calling — nothing has "+
				"yet decided what this credential may do. Add a middleware.RequireScope, or add "+
				"an entry to reviewedUngatedRoutes naming the mechanism in the handler that "+
				"bounds authority by the REQUEST's scopes rather than by the owner's role rows (#339).",
				r.key(), strings.Join(r.chain, " "))
		}
	}
	for key := range reviewedUngatedRoutes {
		if !seen[key] {
			t.Errorf("reviewedUngatedRoutes lists %s, which is no longer an ungated route behind "+
				"requireAuth. Remove the entry so the list keeps meaning what it says — a stale "+
				"exemption is how a real one stops being read.", key)
		}
	}
}

// TestRouterIsTheOnlyRouteTable keeps the route-shape guards COMPLETE.
//
// parseRouterRoutes reads internal/api/router.go and nothing else, so a second
// route table anywhere in the tree would be invisible to it — and every route on
// it would sit outside TestRoutesBehindRequireAuthBindAuthority without anything
// reporting a gap. A guard that silently stops covering part of its subject is
// worse than no guard, because it still reports green.
//
// Today router.go is the whole route surface. If that has to change, this test
// is the place to widen parseRouterRoutes to the new file rather than to exempt
// it.
func TestRouterIsTheOnlyRouteTable(t *testing.T) {
	verbs := map[string]bool{
		"GET": true, "POST": true, "PUT": true, "PATCH": true,
		"DELETE": true, "HEAD": true, "OPTIONS": true,
	}
	fset := token.NewFileSet()
	root := filepath.Join("..", "..")
	var stray []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(path), "../../"))
		if rel == "internal/api/router.go" {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !verbs[sel.Sel.Name] {
				return true
			}
			// A route path is a literal beginning with "/". An HTTP client call
			// takes a URL ("https://...") or a variable, so neither matches.
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !strings.HasPrefix(strings.Trim(lit.Value, `"`), "/") {
				return true
			}
			stray = append(stray, fmt.Sprintf("%s registers %s %s", rel, sel.Sel.Name, lit.Value))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan backend tree: %v", err)
	}
	for _, s := range stray {
		t.Errorf("%s.\n"+
			"parseRouterRoutes only reads internal/api/router.go, so this route is invisible to "+
			"TestRoutesBehindRequireAuthBindAuthority — it could sit behind requireAuth with no "+
			"authority decision and nothing would report it (#339). Widen parseRouterRoutes to "+
			"this file, or move the route into router.go.", s)
	}
}
