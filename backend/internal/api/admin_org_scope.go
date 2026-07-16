// admin_org_scope.go implements the per-organization membership check for the
// /admin/organizations/:id* and /admin/users/:id* routes.
//
// The outer /admin route group (see router.go) already requires the caller's
// flat, GLOBAL admin scope — sourced from GetUserCombinedScopes, which unions
// scopes across EVERY organization the caller belongs to into one org-less set.
// That flat scope says nothing about which organization actually granted it: a
// caller who is admin in just one (possibly low-trust, self-created)
// organization inherits admin globally under that check alone. requireOrgScope
// closes that gap for the routes that name a specific target organization in
// the path, by independently re-deriving the caller's scopes within that exact
// organization (GetUserScopesForOrg) and requiring admin there too.
// requireSharedOrgAdminWithTargetUser closes the same gap for the routes that
// instead name a specific TARGET USER in the path (user CRUD and the GDPR
// export/erase endpoints): it requires the caller to hold admin scope in at
// least one organization the target user also belongs to, rather than
// trusting the flat scope to authorize an action against a user who may
// belong only to a completely unrelated organization.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// requireOrgScope returns a middleware that verifies the authenticated caller
// holds admin scope specifically within the organization named by the request's
// :id path parameter. It re-derives the caller's per-organization scopes via
// OrganizationRepository.GetUserScopesForOrg against the existing flat/global
// JWT's user id — rather than trusting the token's flat scope set, which the
// outer /admin group already checked but which cannot distinguish which
// organization granted the scope.
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
		if !auth.HasScope(scopes, auth.ScopeAdmin) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   "Not authorized for this organization",
				"details": "Caller must hold admin scope within the target organization",
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
