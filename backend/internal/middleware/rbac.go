package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

func contextScopes(c *gin.Context) ([]string, bool) {
	v, ok := c.Get("scopes")
	if !ok {
		return nil, false
	}
	s, ok := v.([]string)
	return s, ok
}

// RequireScope aborts with 403 unless the authenticated user holds the scope.
func RequireScope(scope auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		scopes, ok := contextScopes(c)
		if !ok || !auth.HasScope(scopes, scope) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   "Missing required scope",
				"details": "Required scope: " + string(scope),
			})
			return
		}
		c.Next()
	}
}

// RequireAnyScope aborts with 403 unless the user holds at least one scope.
func RequireAnyScope(scopes ...auth.Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		userScopes, ok := contextScopes(c)
		if !ok || !auth.HasAnyScope(userScopes, scopes) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Missing required scope"})
			return
		}
		c.Next()
	}
}
