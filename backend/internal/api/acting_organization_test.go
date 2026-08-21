package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
	idtenantscope "github.com/sethbacon/terraform-suite-identity/identity/tenantscope"

	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// MUTATIONS THIS FILE IS BUILT TO CATCH:
//
//	an unresolved scope read as "no memberships"  -> TestActingOrganizationTreatsAnUnwiredRouteAsAFault
//	the existence check skipped for an admin      -> TestActingOrganizationRefusesAnAdminsUnknownOrganization
//	the existence check run for everyone          -> TestActingOrganizationDoesNotProbeIdentityForAnOrdinaryCaller
//	a missing verifier proceeding anyway          -> TestActingOrganizationRefusesWhenNothingCanVerify
//	the refusal disclosing whether an org exists  -> TestActingOrganizationRefusalsAreIndistinguishable

type fakeOrgs struct {
	org   *idmodels.Organization
	err   error
	calls int
}

func (f *fakeOrgs) GetByID(_ context.Context, _ string, _ idstore.OrgScope) (*idmodels.Organization, error) {
	f.calls++
	return f.org, f.err
}

func actingCtx(t *testing.T, scope tenantscope.Scope, header string, resolved bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sources", nil)
	if header != "" {
		c.Request.Header.Set(idtenantscope.ActingOrganizationHeader, header)
	}
	if resolved {
		tenantscope.Store(c, scope)
	}
	return c, w
}

func TestActingOrganizationTreatsAnUnwiredRouteAsAFault(t *testing.T) {
	c, w := actingCtx(t, tenantscope.Scope{}, "", false)
	if got := actingOrganization(c, &fakeOrgs{}); got != "" {
		t.Fatalf("got %q from a route with no resolved scope", got)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500. An unresolved scope means middleware.TenantScope was "+
			"never registered — reading it as \"this caller has no memberships\" turns a missing "+
			"router line into a plausible 403 and is the quietest way to reintroduce #393.", w.Code)
	}
}

func TestActingOrganizationAsksForAChoiceWhenThereIsOne(t *testing.T) {
	c, w := actingCtx(t, tenantscope.Scope{OrgIDs: []string{"a", "b"}}, "", true)
	if got := actingOrganization(c, &fakeOrgs{}); got != "" {
		t.Fatalf("got %q; the server chose for the caller", got)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: an unfinished request, not a lack of authority", w.Code)
	}
	if !contains(w.Body.String(), idtenantscope.ActingOrganizationHeader) {
		t.Errorf("the refusal does not name the header the caller must send: %s", w.Body.String())
	}
}

func TestActingOrganizationImpliesTheOnlyOrganization(t *testing.T) {
	orgs := &fakeOrgs{}
	c, w := actingCtx(t, tenantscope.Scope{OrgIDs: []string{"only"}}, "", true)
	if got := actingOrganization(c, orgs); got != "only" {
		t.Fatalf("got %q, want only (status %d, body %s)", got, w.Code, w.Body.String())
	}
}

// An ordinary caller's organization came out of their own memberships, so it
// exists by construction. Probing identity again would be redundant AND would
// put a platform-wide read on the ordinary write path.
func TestActingOrganizationDoesNotProbeIdentityForAnOrdinaryCaller(t *testing.T) {
	orgs := &fakeOrgs{}
	c, _ := actingCtx(t, tenantscope.Scope{OrgIDs: []string{"a", "b"}}, "b", true)
	if got := actingOrganization(c, orgs); got != "b" {
		t.Fatalf("got %q, want b", got)
	}
	if orgs.calls != 0 {
		t.Errorf("identity was probed %d times for an ordinary caller", orgs.calls)
	}
}

// The case the survey found reachable: Permits returns true for ANY id when the
// caller is a platform admin, so without this check they can stamp a row into an
// organization that names nothing — well-formed under Phase 4's NOT NULL,
// invisible to every tenant, and impossible to give to anyone.
func TestActingOrganizationRefusesAnAdminsUnknownOrganization(t *testing.T) {
	orgs := &fakeOrgs{org: nil}
	c, w := actingCtx(t, tenantscope.Scope{PlatformAdmin: true}, "99999999-9999-4999-8999-999999999999", true)

	if got := actingOrganization(c, orgs); got != "" {
		t.Fatalf("a platform admin stamped %q, an organization nothing confirmed exists", got)
	}
	if orgs.calls != 1 {
		t.Errorf("identity probed %d times, want 1", orgs.calls)
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestActingOrganizationAcceptsAnAdminsRealOrganization(t *testing.T) {
	orgs := &fakeOrgs{org: &idmodels.Organization{ID: "real"}}
	c, w := actingCtx(t, tenantscope.Scope{PlatformAdmin: true}, "real", true)
	if got := actingOrganization(c, orgs); got != "real" {
		t.Fatalf("got %q (status %d, %s)", got, w.Code, w.Body.String())
	}
}

func TestActingOrganizationRefusesWhenNothingCanVerify(t *testing.T) {
	c, w := actingCtx(t, tenantscope.Scope{PlatformAdmin: true}, "some-org", true)
	if got := actingOrganization(c, nil); got != "" {
		t.Fatalf("got %q with no verifier wired; stamping an unconfirmed id is the orphan case", got)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// A caller who can tell "no such organization" from "not yours" can enumerate
// the deployment's organizations with a uuid generator.
func TestActingOrganizationRefusalsAreIndistinguishable(t *testing.T) {
	c1, w1 := actingCtx(t, tenantscope.Scope{OrgIDs: []string{"mine"}}, "not-mine", true)
	actingOrganization(c1, &fakeOrgs{})

	c2, w2 := actingCtx(t, tenantscope.Scope{PlatformAdmin: true}, "does-not-exist", true)
	actingOrganization(c2, &fakeOrgs{org: nil})

	if w1.Code != w2.Code {
		t.Errorf("status differs: not-permitted=%d, does-not-exist=%d", w1.Code, w2.Code)
	}
	if w1.Body.String() != w2.Body.String() {
		t.Errorf("body differs, so a caller can distinguish the two:\n  not-permitted:  %s\n  does-not-exist: %s",
			w1.Body.String(), w2.Body.String())
	}
	for _, body := range []string{w1.Body.String(), w2.Body.String()} {
		if contains(body, "not-mine") || contains(body, "does-not-exist") {
			t.Errorf("the refusal echoes the caller-supplied id back: %s", body)
		}
	}
}

func TestActingOrganizationSurfacesAVerifierFailure(t *testing.T) {
	orgs := &fakeOrgs{err: errors.New("identity is unreachable")}
	c, w := actingCtx(t, tenantscope.Scope{PlatformAdmin: true}, "some-org", true)
	if got := actingOrganization(c, orgs); got != "" {
		t.Fatalf("got %q despite the verifier failing", got)
	}
	if w.Code == http.StatusOK {
		t.Error("a verifier failure was treated as success")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}
