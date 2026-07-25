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
	"context"
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

// adminOrgSet returns the set of organization IDs in which callerID holds admin
// scope. It underpins per-org narrowing of the global admin READ lists (users,
// API keys): because being admin in a single organization yields the flat
// ScopeAdmin that passes the outer /admin gate, those lists must be filtered to
// the organizations the caller actually administers rather than exposing every
// tenant's records (#182). An empty callerID or no admin membership yields an
// empty set (the caller then sees nothing cross-org).
func adminOrgSet(ctx context.Context, orgRepo *idstore.OrganizationRepository, callerID string) (map[string]struct{}, error) {
	orgs := map[string]struct{}{}
	if callerID == "" {
		return orgs, nil
	}
	memberships, err := orgRepo.GetUserMemberships(ctx, callerID)
	if err != nil {
		return nil, err
	}
	for _, m := range memberships {
		if auth.HasScope(m.RoleTemplateScopes, auth.ScopeAdmin) {
			orgs[m.OrganizationID] = struct{}{}
		}
	}
	return orgs, nil
}
