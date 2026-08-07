// admin_org_scope.go implements the per-organization membership check for the
// /admin/organizations/:id* and /admin/users/:id* routes.
//
// /admin/organizations/:id* sits behind organizations:read/:create at the
// group level (see router.go) — neither of which names a specific target
// organization — so requireOrgScope independently re-derives the caller's
// scopes within that exact organization (GetUserScopesForOrg) and requires
// organizations:write (or admin) there too. This lets an org_owner (holds
// organizations:write, never the flat admin wildcard) fully manage the
// organization it owns, while still closing the cross-org privilege escalation
// a caller who is only organizations:write/admin in ONE organization would
// otherwise get against every OTHER organization.
// requireSharedOrgAdminWithTargetUser closes the analogous gap for the routes
// that instead name a specific TARGET USER in the path (user CRUD and the GDPR
// export/erase endpoints, still gated on the outer /admin group's flat, GLOBAL
// admin scope — see router.go): it requires the caller to hold admin scope in
// at least one organization the target user also belongs to, rather than
// trusting the flat scope to authorize an action against a user who may belong
// only to a completely unrelated organization.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// requireOrgScope returns a middleware that verifies the authenticated caller
// holds organizations:write (or admin) scope specifically within the
// organization named by the request's :id path parameter. It re-derives the
// caller's per-organization scopes via OrganizationRepository.GetUserScopesForOrg
// against the existing flat/global JWT's user id — rather than trusting any
// flat scope set, which cannot distinguish which organization granted it.
func (h *AdminHandlers) requireOrgScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")
		callerID, _ := c.Get("user_id")
		uid, _ := callerID.(string)
		if orgID == "" || uid == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "organization membership could not be verified"})
			return
		}

		scopes, err := h.orgRepo.GetUserScopesForOrg(c.Request.Context(), uid, orgID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify organization membership"})
			return
		}
		if !(auth.HasScope(scopes, auth.ScopeOrganizationsWrite) || auth.HasScope(scopes, auth.ScopeAdmin)) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   "Not authorized for this organization",
				"details": "Caller must hold organizations:write (or admin) scope within the target organization",
			})
			return
		}
		c.Next()
	}
}

// requireSharedOrgAdminWithTargetUser returns a middleware that verifies the
// authenticated caller holds admin scope in at least one organization that the
// target user (the request's :id path parameter) also belongs to. Without
// this check, the outer /admin group's flat ScopeAdmin lets an admin of ANY
// single organization act on ANY user in the system — update/delete their
// account, read their organization memberships, or export/erase their data
// under GDPR — regardless of whether that user has any relationship to the
// caller's organization at all.
//
// A target user with NO organization memberships (e.g. a pre-provisioned or
// orphaned account not yet linked to any tenant) is allowed through: there is
// no cross-tenant boundary to violate, since the user isn't a member of any
// organization other admins could improperly reach into.
func (h *AdminHandlers) requireSharedOrgAdminWithTargetUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		targetUserID := c.Param("id")
		callerID, _ := c.Get("user_id")
		uid, _ := callerID.(string)
		if targetUserID == "" || uid == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "target user's organization membership could not be verified"})
			return
		}

		memberships, err := h.orgRepo.GetUserMemberships(c.Request.Context(), targetUserID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to load target user's organization memberships"})
			return
		}
		if len(memberships) == 0 {
			// No organization ties for this user at all — nothing cross-tenant to
			// protect against.
			c.Next()
			return
		}

		for _, m := range memberships {
			scopes, err := h.orgRepo.GetUserScopesForOrg(c.Request.Context(), uid, m.OrganizationID)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to verify organization membership"})
				return
			}
			if auth.HasScope(scopes, auth.ScopeAdmin) {
				c.Next()
				return
			}
		}

		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":   "Not authorized for this user",
			"details": "Caller must hold admin scope within at least one organization the target user belongs to",
		})
	}
}

// callerScopeFor resolves the organizations in which the authenticated caller's
// ROLE TEMPLATE grants required, as the store.OrgScope every scoped accessor in
// the identity package now demands.
//
// It replaces the hand-rolled adminOrgSet this file carried until identity
// v0.25.0: OrganizationRepository.OrgScopeForUser is that function, shipped by
// the module that owns organization_members, and it also deduplicates and sorts
// the ids so the bound query argument is a function of the SET rather than of
// map iteration order (the reason adminOrgSet's callers had to sort by hand).
//
// There is deliberately NO platform-wide branch here. "Platform-wide" is a
// property of the token, and TSM has no platform-wide principal: admin is
// granted per organization and merely SURFACES as a flat scope, so every
// /admin caller is somebody's tenant admin. See
// TestNoPlatformWideOrgScopeInAuditHandlers, which fails the build if an
// OrgScopeAllOrganizations() ever appears on one of these paths.
func (h *AdminHandlers) callerScopeFor(c *gin.Context, required auth.Scope) (idstore.OrgScope, error) {
	callerID, _ := c.Get("user_id")
	uid, _ := callerID.(string)
	// The zero OrgScope matches nothing, so a caller that ignored the error
	// would still read no rows rather than every tenant's.
	return h.orgRepo.OrgScopeForUser(c.Request.Context(), uid, string(required), auth.ReadWritePairs())
}

// callerOrgScope resolves the tenant constraint every /admin READ behind the
// flat ScopeAdmin gate must carry: the organizations the caller actually
// administers, plus rows that have no owning organization at all.
//
// Applies to every axis behind that gate — the paginated audit list, the
// CSV/JSON audit export, the GDPR per-user export, and the user list/search.
// The gate accepts any single-org admin's flat ScopeAdmin, so it is not a
// tenant boundary and each axis has to narrow for itself (#182/#298, and #331
// for the export that did not).
//
// The unowned axis is not a widening, and it means the right thing on both
// tables this scope is applied to:
//
//   - audit_logs: TSM deliberately writes platform-level events with a NULL
//     organization_id — logins, state-file and source actions (state_sources
//     are a single shared pool with no per-tenant owner), and federated entries
//     whose sibling-provisioned actor cannot be resolved locally
//     (audit_ingest.go). Those events belong to no tenant and every admin is
//     their intended reviewer.
//   - users: a user with NO memberships at all. Letting those through is what
//     requireSharedOrgAdminWithTargetUser already does in Go ("nothing
//     cross-tenant to protect against"), and what the deleted usersInAdminOrgs
//     post-filter did for the list axis.
//
// OrgScopeOrganizations alone would hide both from the people meant to see
// them; OrgScopeAllOrganizations would hand the caller every other tenant's
// rows. A caller who administers no organization gets "unowned rows only" —
// the same result the in-memory filters produced from an empty admin set.
func (h *AdminHandlers) callerOrgScope(c *gin.Context) (idstore.OrgScope, error) {
	scope, err := h.callerScopeFor(c, auth.ScopeAdmin)
	if err != nil {
		return idstore.OrgScope{}, err
	}
	return scope.WithUnowned(), nil
}

// routeOrgScope is the tenancy of a route that names ONE organization in its
// :id path parameter and sits behind requireOrgScope.
//
// UPGRADING.md's rule for this shape is "pass the scope you resolved for that
// guard", and requireOrgScope resolved exactly one organization: it re-derived
// the caller's scopes WITHIN c.Param("id") and required organizations:write (or
// admin) there. Re-running OrgScopeForUser would answer a wider question (every
// organization the caller can write) than the guard asked, and would cost a
// second membership read per request to do it.
func routeOrgScope(c *gin.Context) idstore.OrgScope {
	return idstore.OrgScopeOrganizations(c.Param("id"))
}
