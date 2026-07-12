// admin_org_scope.go implements the per-organization membership check for the
// /admin/organizations/:id* routes.
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
