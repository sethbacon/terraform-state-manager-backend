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

		// emailVerified=true: this is the hardcoded dev-only bootstrap account,
		// not an externally-asserted identity — there is no lower-trust claim to
		// gate here. Reachability is dev-mode-only in two independent places:
		// the route itself is only registered when auth.IsDevMode() (see
		// router.go), and this handler re-checks auth.IsDevMode() above and
		// returns 403 otherwise, so this path cannot run in production even if
		// the route registration guard were ever bypassed.
		user, err := h.userRepo.GetOrCreateUserFromOIDC(ctx, "dev:admin", "admin.user@local.dev", "Dev Admin", true)
		if err != nil {
			serverError(c, err, "Failed to create dev user")
			return
		}
		h.assignRole(ctx, user.ID, "admin")

		scopes, err := h.orgRepo.GetUserCombinedScopes(ctx, user.ID)
		if err != nil {
			scopes = []string{}
		}
		token, err := auth.GenerateJWT(user.ID, user.Email, scopes, sessionTTL)
		if err != nil {
			serverError(c, err, "Failed to generate token")
			return
		}
		h.setSessionCookies(c, token)
		c.JSON(http.StatusOK, gin.H{"expires_in": int(sessionTTL.Seconds())})
	}
}
