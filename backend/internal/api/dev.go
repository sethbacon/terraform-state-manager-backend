// dev.go implements the development-only login that bypasses the IdP. It is only
// registered when DEV_MODE is enabled.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// DevLoginHandler provisions (or reuses) a built-in dev admin user, assigns the
// admin role in the default organization, and issues an HttpOnly session cookie.
// The JWT is never returned in the body (cookie-only sessions).
// POST /api/v1/dev/login
func (h *AuthHandlers) DevLoginHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !auth.IsDevMode() {
			c.JSON(http.StatusForbidden, gin.H{"error": "Dev login is disabled"})
			return
		}
		ctx := c.Request.Context()

		// The email is a fixed, server-owned constant (not an externally-asserted
		// claim), so it is treated as verified.
		user, err := h.userRepo.GetOrCreateUserByOIDC(ctx, "dev:admin", "admin.user@local.dev", "Dev Admin", true)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create dev user"})
			return
		}
		h.assignRole(ctx, user.ID, "admin")

		scopes, err := h.orgRepo.GetUserCombinedScopes(ctx, user.ID)
		if err != nil {
			scopes = []string{}
		}
		token, err := auth.GenerateJWT(user.ID, user.Email, scopes, sessionTTL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
			return
		}
		h.setSessionCookies(c, token)
		c.JSON(http.StatusOK, gin.H{"expires_in": int(sessionTTL.Seconds())})
	}
}
