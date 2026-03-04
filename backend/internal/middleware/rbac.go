package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// RequireScope returns a gin.HandlerFunc that aborts with 403 Forbidden if
// the authenticated user does not possess the given scope.
// The user's scopes are expected to be stored under the "scopes" key in the
// gin context (set by AuthMiddleware).
func RequireScope(scope auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		userScopes := getScopesFromContext(c)
		if userScopes == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "no scopes found in context",
			})
			return
		}

		if !auth.HasScope(userScopes, scope) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":          "insufficient permissions",
				"required_scope": string(scope),
			})
			return
		}

		c.Next()
	}
}

// RequireAnyScope returns a gin.HandlerFunc that aborts with 403 Forbidden
// unless the authenticated user possesses at least one of the provided scopes.
func RequireAnyScope(scopes ...auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		userScopes := getScopesFromContext(c)
		if userScopes == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "no scopes found in context",
			})
			return
		}

		if !auth.HasAnyScope(userScopes, scopes) {
			scopeStrs := make([]string, len(scopes))
			for i, s := range scopes {
				scopeStrs[i] = string(s)
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":           "insufficient permissions",
				"required_scopes": scopeStrs,
				"message":         "at least one of the required scopes is needed",
			})
			return
		}

		c.Next()
	}
}

// RequireAllScopes returns a gin.HandlerFunc that aborts with 403 Forbidden
// unless the authenticated user possesses every one of the provided scopes.
func RequireAllScopes(scopes ...auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		userScopes := getScopesFromContext(c)
		if userScopes == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "no scopes found in context",
			})
			return
		}

		if !auth.HasAllScopes(userScopes, scopes) {
			scopeStrs := make([]string, len(scopes))
			for i, s := range scopes {
				scopeStrs[i] = string(s)
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":           "insufficient permissions",
				"required_scopes": scopeStrs,
				"message":         "all of the required scopes are needed",
			})
			return
		}

		c.Next()
	}
}

// RequireOrgMembership returns a gin.HandlerFunc that verifies the
// authenticated user is a member of the organization identified by the
// "org_id" path parameter. The membership check is delegated to
// orgRepo.GetMember.
func RequireOrgMembership(orgRepo *repositories.OrganizationRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := c.Get("user_id")
		if !exists {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "authentication required",
			})
			return
		}

		uid, ok := userID.(string)
		if !ok || uid == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "invalid user identity",
			})
			return
		}

		orgID := c.Param("org_id")
		if orgID == "" {
			// Also try from context (set by auth middleware for API keys).
			if ctxOrgID, ok := c.Get("org_id"); ok {
				orgID, _ = ctxOrgID.(string)
			}
		}
		if orgID == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "organization ID is required",
			})
			return
		}

		member, err := orgRepo.GetMember(c.Request.Context(), orgID, uid)
		if err != nil || member == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":  "not a member of this organization",
				"org_id": orgID,
			})
			return
		}

		c.Next()
	}
}

// RequireOrgScope returns a gin.HandlerFunc that checks both the required
// scope AND organization membership. It is a convenience combination of
// RequireScope and RequireOrgMembership.
func RequireOrgScope(scope auth.Scope, orgRepo *repositories.OrganizationRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check scope first.
		userScopes := getScopesFromContext(c)
		if userScopes == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "no scopes found in context",
			})
			return
		}

		if !auth.HasScope(userScopes, scope) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":          "insufficient permissions",
				"required_scope": string(scope),
			})
			return
		}

		// Check organization membership.
		userID, exists := c.Get("user_id")
		if !exists {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "authentication required",
			})
			return
		}

		uid, ok := userID.(string)
		if !ok || uid == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "invalid user identity",
			})
			return
		}

		orgID := c.Param("org_id")
		if orgID == "" {
			if ctxOrgID, ok := c.Get("org_id"); ok {
				orgID, _ = ctxOrgID.(string)
			}
		}
		if orgID == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "organization ID is required",
			})
			return
		}

		member, err := orgRepo.GetMember(c.Request.Context(), orgID, uid)
		if err != nil || member == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":  "not a member of this organization",
				"org_id": orgID,
			})
			return
		}

		c.Next()
	}
}

// getScopesFromContext extracts the user's scopes from the gin context.
func getScopesFromContext(c *gin.Context) []string {
	raw, exists := c.Get("scopes")
	if !exists {
		return nil
	}

	scopes, ok := raw.([]string)
	if !ok {
		return nil
	}

	return scopes
}
