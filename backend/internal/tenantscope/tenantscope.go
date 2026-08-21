// Package tenantscope resolves, once per request, the set of organizations a
// caller is allowed to reach — the dimension TSM's authorization has never had.
//
// PHASE 2a OF FOUR of #393. Phase 1 (internal/tenancy) gave nine root tables a
// nullable organization_id and stamped every row. It filtered nothing, on
// purpose, and said why: "nothing in a TSM request knows which organization the
// caller is acting as". This package is that missing thing. It still filters
// nothing — no repository signature changes here — but from this point a handler
// CAN ask "which organizations is this request for?", which Phase 3 needs before
// it can flip a single read.
//
// # What is actually broken, and it is not a missing WHERE clause
//
// internal/approles/reads.go:GetUserCombinedScopes flattens every membership's
// role-template scopes into one set and discards the organization each came
// from; middleware.RequireScope then tests that flat set with no organization
// dimension at all. So `state:read` held in ONE organization authorizes reads of
// EVERY organization's data. internal/tenancy/isolation_integration_test.go
// demonstrates the consequence against a real database rather than arguing it
// from the call graph.
//
// The flattening is not a bug in GetUserCombinedScopes — a session JWT is one
// flat scope list by construction, and that is fine for "may this credential
// read state at all". It is only wrong as an answer to "whose state", and
// nothing in the codebase asked the second question because there was no type in
// which to phrase it. There is now, and it is the zero value of that type that
// matters most: it permits nothing.
//
// # Mirrored from terraform-registry, deliberately, and where it diverges
//
// terraform-registry's internal/tenantscope is the same package solving the same
// class, and this is its shape — Scope{PlatformAdmin, OrgIDs}, Permits/
// PermitsPtr/Empty, a Resolve that fails closed. Two divergences are forced by
// facts about TSM, and both are documented at the code that makes them:
//
//  1. PLATFORM ADMIN COMES FROM THE CARRIER, NOT FROM THE FLAT SCOPE. In
//     registry, `admin` in a session IS the platform-wide wildcard, so reading
//     it off the context is reading platform-adminness. In TSM it is not: `admin`
//     is granted per organization through an admin-bearing role template and
//     merely SURFACES flat, which is why internal/api/admin_org_scope.go states
//     that "every /admin caller is somebody's tenant admin" and has no
//     platform-wide branch. Deriving PlatformAdmin from the flat scope here would
//     hand every single-organization admin the whole estate — #393's leak, rebuilt
//     inside the type meant to close it. See Resolve.
//
//  2. NO API-KEY ORGANIZATION BRANCH. registry's Resolve trusts an API key's
//     organization_id as an authority binding. TSM's is not one: mintKey stamps
//     every key with the global default organization whoever owns it, which
//     internal/api/apikeys.go:keyScope documents at length. The tenant boundary
//     for a TSM key is its OWNER's membership, so keys take the same membership
//     path as sessions here. Copying registry's branch would have bound every key
//     in the deployment to the default organization.
//
// # The zero value permits nothing
//
// Every failure path in this package returns it. A caller with no principal, a
// resolver that was not wired, a principal with no qualifying membership: all
// select nothing rather than everything. The one thing that is NOT reported as
// an empty scope is a lookup that FAILED — that comes back as an error, so the
// handler answers 500 instead of quietly serving a caller their own empty world
// (or, if a future caller inverts the test, everyone else's).
package tenantscope

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// Memberships resolves the organizations in which a principal holds a scope.
//
// *approles.Members satisfies it, and is the only production implementation:
// OrgScopeForUser there keeps exactly the memberships whose ROLE TEMPLATE grants
// `required`, under this application's role source, with this application's
// write-implies-read pairs. That is already the resolver every scoped /admin
// route is built on (internal/api/admin_org_scope.go:callerScopeFor), so
// tenancy resolved here and tenancy resolved there cannot disagree.
//
// It is an interface and not the concrete type for one reason, and it is a test
// reason rather than an abstraction reason: the failure mode this package must
// get right is a membership lookup that ERRORS, and *approles.Members can only
// be made to error by taking a database away from it. A one-method seam lets
// that case be a table row instead of an integration fixture.
type Memberships interface {
	OrgScopeForUser(ctx context.Context, userID, required string, rwPairs idauth.ReadWritePairs) (idstore.OrgScope, error)
}

// PlatformAdmins answers whether a principal holds platform-wide authority right
// now. *platformadmin.Service satisfies it.
//
// LIVE, NOT CACHED, and that is the carrier's whole point — see
// platformadmin.elevate's caller in internal/middleware/auth.go: a row removed
// from platform_admins stops elevating on the NEXT request rather than whenever
// the holder's longest session expires. A scope resolved from a cached answer
// would reintroduce exactly that window on the read path.
type PlatformAdmins interface {
	IsPlatformAdmin(ctx context.Context, userID string) (bool, error)
}

// Scope is the set of organizations a request may reach.
//
// The zero value denies everything, so a handler that fails to resolve a scope —
// or resolves one for a principal with no qualifying membership — selects
// nothing rather than the whole deployment.
type Scope struct {
	// PlatformAdmin marks a caller holding a platform_admins carrier row. That
	// authority is genuinely platform-wide: it is TSM's only principal that is
	// not somebody's tenant admin, which is why it is the only thing here that
	// crosses an organization boundary.
	//
	// It is NOT "the caller's scopes contain admin". See Resolve.
	PlatformAdmin bool
	// OrgIDs are the organizations in which the caller was verified to hold the
	// scope the route required — not merely the organizations they belong to.
	// That distinction is the point of the type: membership answers "do you know
	// these people", and the question a read has to ask is "may you do THIS
	// here".
	OrgIDs []string
}

// Permits reports whether a row owned by orgID is inside the scope.
//
// UNOWNED ROWS. An empty orgID means a row whose organization_id is NULL, and it
// is permitted only for a platform admin. NULL on these nine tables means "no
// tenant has been asserted", not "belongs to everyone": internal/tenancy's
// backfill exists precisely because a NULL there is a row that predates the
// column, and it stamps them with the DEFAULT organization — so a NULL seen
// after Phase 1 is a row written by a replica still on the previous build, whose
// true owner is the default organization and whose contents are therefore
// somebody's. Admitting it to everyone would leak exactly the tenant that owns
// the most rows.
//
// This deliberately differs from the answer internal/api/admin_org_scope.go
// gives on audit_logs and users, where WithUnowned() admits unowned rows to
// every admin. That is not an inconsistency, it is the same rule applied to
// different data: TSM WRITES platform-level audit entries with a NULL
// organization_id on purpose, and a user with no membership belongs to no
// tenant. Neither is true of a state source or a drift run — there is no such
// thing as a platform-level state file.
func (s Scope) Permits(orgID string) bool {
	if s.PlatformAdmin {
		return true
	}
	if orgID == "" {
		return false
	}
	for _, id := range s.OrgIDs {
		if id == orgID {
			return true
		}
	}
	return false
}

// PermitsPtr is Permits for the nullable organization_id the nine partitioned
// tables actually carry, and will carry until Phase 4 makes the column NOT NULL.
// A nil pointer is the NULL owner Permits declines to anybody but a platform
// admin.
func (s Scope) PermitsPtr(orgID *string) bool {
	if orgID == nil {
		return s.Permits("")
	}
	return s.Permits(*orgID)
}

// Empty reports whether the scope can select nothing at all.
func (s Scope) Empty() bool { return !s.PlatformAdmin && len(s.OrgIDs) == 0 }

// Resolve resolves the caller's tenant scope for the scope a route requires.
//
// It fails closed: a missing or malformed principal yields an empty scope, an
// unwired resolver yields an empty scope, and a lookup FAILURE is returned as an
// error so the handler can 500 rather than silently widening to every
// organization — or, just as bad, narrowing a platform administrator to nothing
// during the incident in which they need the read.
//
// GUARD tenant-scope-resolver (#393).
//
// # Why the carrier and not the flat `admin` scope
//
// GUARD tenant-scope-platform-admin (#393). terraform-registry resolves this
// from the context's scope list, because there `admin` is the platform-wide
// wildcard. Transplanting that line into TSM would be the leak this package
// closes, written into the package that closes it: TSM grants `admin` through a
// role template held IN AN ORGANIZATION (auth.AppRoleTemplates), the session JWT
// flattens every membership's scopes into one list (approles.GetUserCombinedScopes),
// and so an admin of one tenant arrives carrying a flat `admin` that says
// nothing about the other tenants. internal/api/admin_org_scope.go already
// refuses to read it that way — "every /admin caller is somebody's tenant admin"
// — and requireOrgScope re-derives per-organization authority for that reason.
//
// The carrier (platform_admins, migration 000030) is the one principal that IS
// platform-wide, and it is a live read of a table an operator can empty.
//
// # Why the carrier is not consulted for an API key
//
// GUARD tenant-scope-key-no-elevation (#393, and #223 before it).
// middleware.authenticateAPIKey takes no platformadmin.Service — its doc calls
// that structural rather than remembered: "a key must not inherit its owner's
// platform-admin" is enforced by there being nothing there to call, because a
// key is a long-lived, frequently unattended CI credential and an elevation
// riding along on it would be revocable only by deleting the key. A carrier read
// keyed on the OWNER's user id here would reinstate precisely that inheritance
// one layer further in, on the read path, where it would be much harder to see.
// So on an API-key request the carrier is not asked, and the key's tenancy is
// its owner's memberships — the boundary internal/api/apikeys.go:keyScope
// already names as the right one for a TSM key.
func Resolve(c *gin.Context, memberships Memberships, admins PlatformAdmins, required auth.Scope) (Scope, error) {
	if c == nil {
		return Scope{}, nil
	}

	userID := principal(c)
	if userID == "" {
		// No principal at all: an unauthenticated request, or an mTLS client
		// (internal/auth/mtls sets scopes and auth_method but never a user_id,
		// because a certificate subject is not an identity user and has no
		// memberships to resolve). Either way there is nobody whose tenancy could
		// be looked up, and the empty scope is the honest answer.
		return Scope{}, nil
	}

	if !isAPIKeyRequest(c) && admins != nil {
		isAdmin, err := admins.IsPlatformAdmin(requestContext(c), userID)
		switch {
		case errors.Is(err, platformadmin.ErrNotConfigured):
			// No carrier is wired — a deployment without a database connection, or
			// a test rig. Not an error, and not an elevation: middleware.elevate
			// treats an absent carrier the same way, because a carrier that is not
			// there withholds authority rather than granting it. Fall through to
			// memberships.
		case err != nil:
			// An authority question that did not resolve is not a completed "no".
			// Returning the zero scope WITH the error keeps a caller that ignores
			// the error selecting nothing.
			return Scope{}, err
		case isAdmin:
			return Scope{PlatformAdmin: true}, nil
		}
	}

	if memberships == nil {
		// A handler wired without a membership resolver cannot verify anything.
		// Denying is the only safe answer; returning an unfiltered result would be
		// the defect this package exists to close.
		return Scope{}, nil
	}

	// GUARD tenant-scope-role-template (#393): membership alone is not authority.
	// OrgScopeForUser keeps only the organizations whose ROLE TEMPLATE grants
	// `required`, under this application's write-implies-read pairs — so a viewer
	// in an organization does not acquire the tenancy of an editor in it. This is
	// the same call internal/api/admin_org_scope.go:callerScopeFor makes for the
	// /admin reads, deliberately: a second implementation of one tenancy decision
	// is the thing that drifts.
	orgScope, err := memberships.OrgScopeForUser(requestContext(c), userID, string(required), auth.ReadWritePairs())
	if err != nil {
		return Scope{}, err
	}
	return Scope{OrgIDs: orgScope.OrganizationIDs()}, nil
}

// principal returns the authenticated user id the auth middleware published, or
// "" when there is none or it is not a string.
//
// A principal that cannot be interpreted is a principal that cannot be
// authorized, so a non-string value under the key is "" rather than a panic or a
// pass.
func principal(c *gin.Context) string {
	v, ok := c.Get("user_id")
	if !ok {
		return ""
	}
	id, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(id)
}

// isAPIKeyRequest reports whether this request authenticated with an API key.
//
// It reads auth_method, the value middleware.authenticateAPIKey publishes,
// rather than probing for api_key_id: auth_method is the key every other
// consumer already reads (AuthMiddleware's own mTLS short-circuit,
// internal/api's audit vocabulary), and one fact should have one name.
func isAPIKeyRequest(c *gin.Context) bool {
	v, ok := c.Get("auth_method")
	if !ok {
		return false
	}
	method, ok := v.(string)
	return ok && method == "apikey"
}

// requestContext returns the request's context, or a background context for the
// synthetic gin.Context a test or a non-HTTP caller may hand in. A resolver that
// panicked on a nil Request would fail OPEN in the worst way: the request would
// never reach the code that decides whether it is permitted.
func requestContext(c *gin.Context) context.Context {
	if c.Request == nil {
		return context.Background()
	}
	return c.Request.Context()
}
