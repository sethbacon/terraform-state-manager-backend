package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// MUTATION-VERIFIED, not merely green. Each of these was run against a
// deliberately broken TenantScope before landing:
//
//	stores a scope even when Resolve errored -> TestTenantScopeResolverFailure...
//	aborts instead of continuing on error    -> TestTenantScopeResolverFailure...
//	passes a hard-coded scope to Resolve     -> TestTenantScopeAsksForTheRoutesScope
//	stores nothing on the success path       -> TestTenantScopePublishesTheScope,
//	                                            TestTenantScopeNoPrincipalIsEmptyNotAbsent
//
// A change here that leaves one of those mutations passing has removed a guard.

const (
	scopeOrgA  = "11111111-1111-4111-8111-111111111111"
	scopeOrgB  = "22222222-2222-4222-8222-222222222222"
	scopeUser  = "user-1"
	scopeRoute = "/scoped"
)

var errScopeLookup = errors.New("membership store is unreachable")

type stubMemberships struct {
	scope       idstore.OrgScope
	err         error
	calls       int
	gotRequired string
}

func (s *stubMemberships) OrgScopeForUser(_ context.Context, _, required string, _ idauth.ReadWritePairs) (idstore.OrgScope, error) {
	s.calls++
	s.gotRequired = required
	return s.scope, s.err
}

type stubAdmins struct {
	admin bool
	err   error
}

func (s *stubAdmins) IsPlatformAdmin(context.Context, string) (bool, error) {
	return s.admin, s.err
}

// scopedRouter runs TenantScope behind a stand-in for AuthMiddleware and records
// what the handler saw. observed is nil when the handler never ran at all, which
// is how "did it abort?" is distinguished from "did it store nothing?".
type observation struct {
	ran      bool
	scope    tenantscope.Scope
	resolved bool
}

func runTenantScope(t *testing.T, userID string, m tenantscope.Memberships, a tenantscope.PlatformAdmins, required auth.Scope) (*observation, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	obs := &observation{}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if userID != "" {
			c.Set("user_id", userID)
			// What AuthMiddleware publishes alongside user_id on the session
			// path. tenantscope maps auth_method explicitly and defaults to the
			// NARROW reading, so a synthetic context that omits it is not a
			// session and is never elevated — correct, and not what these cases
			// are about.
			c.Set("auth_method", "jwt")
		}
		c.Next()
	})
	r.GET(scopeRoute, TenantScope(m, a, required), func(c *gin.Context) {
		obs.ran = true
		obs.scope, obs.resolved = tenantscope.FromContext(c)
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, scopeRoute, nil))
	return obs, w.Code
}

func TestTenantScopePublishesTheScope(t *testing.T) {
	m := &stubMemberships{scope: idstore.OrgScopeOrganizations(scopeOrgA)}
	obs, code := runTenantScope(t, scopeUser, m, &stubAdmins{}, auth.ScopeStateRead)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !obs.resolved {
		t.Fatal("the handler saw no scope; the middleware resolved one and did not publish it")
	}
	if !obs.scope.Permits(scopeOrgA) {
		t.Fatalf("scope does not permit the organization the resolver returned: %+v", obs.scope)
	}
	if obs.scope.Permits(scopeOrgB) {
		t.Fatalf("scope permits an organization the resolver did not return: %+v", obs.scope)
	}
}

// The scope handed to a route is only meaningful for the authority that route
// demands, so the middleware must ask for the scope it was registered with —
// not a fixed one. A middleware that always asked for state:read would, in
// Phase 3, give the sources:manage routes a tenancy derived from read authority.
func TestTenantScopeAsksForTheRoutesScope(t *testing.T) {
	for _, required := range []auth.Scope{auth.ScopeStateRead, auth.ScopeSourcesManage} {
		m := &stubMemberships{scope: idstore.OrgScopeOrganizations(scopeOrgA)}
		if _, code := runTenantScope(t, scopeUser, m, &stubAdmins{}, required); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if m.calls != 1 {
			t.Fatalf("resolver called %d times, want exactly 1", m.calls)
		}
		if m.gotRequired != string(required) {
			t.Fatalf("resolver was asked for %q, want %q", m.gotRequired, required)
		}
	}
}

// A caller with no principal is FAIL-CLOSED, and the distinction matters: the
// scope IS resolved (there is nobody, so the answer is "nothing"), which reads
// no rows. It is not an unresolved scope, which is an unanswered question.
func TestTenantScopeNoPrincipalIsEmptyNotAbsent(t *testing.T) {
	m := &stubMemberships{scope: idstore.OrgScopeOrganizations(scopeOrgA)}
	obs, code := runTenantScope(t, "", m, &stubAdmins{}, auth.ScopeStateRead)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !obs.resolved {
		t.Fatal("a principal-less request must resolve to the empty scope, not to no scope")
	}
	if !obs.scope.Empty() {
		t.Fatalf("a principal-less request resolved to %+v; it must permit nothing", obs.scope)
	}
	if m.calls != 0 {
		t.Fatalf("the membership store was queried %d times for a request with no principal", m.calls)
	}
}

// GUARD tenant-scope-observe-only (#393). A resolver failure must NOT abort in
// Phase 2b: nothing reads the scope to decide anything yet, so a 500 here would
// deny a read the system can still serve correctly. The failure is carried as
// "no scope resolved" instead — which Phase 3 must turn into a 500 at the
// reading callers, and which must never be mistaken for an empty scope.
func TestTenantScopeResolverFailureDoesNotAbortAndStoresNothing(t *testing.T) {
	m := &stubMemberships{scope: idstore.OrgScopeAllOrganizations(), err: errScopeLookup}
	obs, code := runTenantScope(t, scopeUser, m, &stubAdmins{}, auth.ScopeStateRead)

	if code != http.StatusOK {
		t.Fatalf("status = %d; Phase 2b must not fail a read on a resolver error", code)
	}
	if !obs.ran {
		t.Fatal("the handler never ran; the middleware aborted on a resolver error")
	}
	if obs.resolved {
		t.Fatalf("a failed lookup was published as a scope (%+v); an unanswered question "+
			"must not become an answer", obs.scope)
	}
}

// The carrier answers, and it is the ONLY thing in TSM that crosses an
// organization boundary — never the flat `admin` scope.
func TestTenantScopePlatformAdmin(t *testing.T) {
	m := &stubMemberships{scope: idstore.OrgScopeOrganizations(scopeOrgA)}
	obs, _ := runTenantScope(t, scopeUser, m, &stubAdmins{admin: true}, auth.ScopeStateRead)

	if !obs.resolved || !obs.scope.PlatformAdmin {
		t.Fatalf("platform admin resolved to %+v (resolved=%v)", obs.scope, obs.resolved)
	}
	if m.calls != 0 {
		t.Fatalf("the membership store was queried %d times for a platform admin", m.calls)
	}
}

// A deployment with no carrier wired — the unit-test rig, or a server without an
// identity connection — hands a NIL *platformadmin.Service to this middleware.
// It must fall through to memberships rather than panic or elevate: an absent
// carrier withholds authority, it does not grant it.
func TestTenantScopeNilCarrierFallsThroughToMemberships(t *testing.T) {
	var absent *platformadmin.Service
	m := &stubMemberships{scope: idstore.OrgScopeOrganizations(scopeOrgA)}
	obs, code := runTenantScope(t, scopeUser, m, absent, auth.ScopeStateRead)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !obs.resolved || obs.scope.PlatformAdmin {
		t.Fatalf("an absent carrier produced %+v (resolved=%v); it must neither elevate nor abort",
			obs.scope, obs.resolved)
	}
	if !obs.scope.Permits(scopeOrgA) {
		t.Fatalf("scope did not fall through to memberships: %+v", obs.scope)
	}
}
