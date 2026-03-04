package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// SetupTokenMiddleware returns a gin.HandlerFunc that gates initial setup
// endpoints behind a one-time setup token. The token is transmitted in the
// Authorization header with the "SetupToken" scheme:
//
//	Authorization: SetupToken <token>
//
// The middleware validates the token against a bcrypt hash stored in the
// system_settings table (retrieved via oidcConfigRepo.GetSetupTokenHash).
// Once setup is marked as completed (oidcConfigRepo.IsSetupCompleted) all
// further requests are rejected with 403 Forbidden.
func SetupTokenMiddleware(oidcConfigRepo *repositories.OIDCConfigRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check whether setup has already been completed.
		completed, err := oidcConfigRepo.IsSetupCompleted(c.Request.Context())
		if err != nil {
			slog.Error("TSM setup: failed to check setup status", "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "failed to check setup status",
			})
			return
		}
		if completed {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   "setup already completed",
				"message": "The initial setup has already been completed. This endpoint is no longer available.",
			})
			return
		}

		// Extract the setup token from the Authorization header.
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "SetupToken ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "setup token required",
				"message": "Provide the setup token in the Authorization header: SetupToken <token>",
			})
			return
		}

		token := strings.TrimSpace(strings.TrimPrefix(authHeader, "SetupToken "))
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "empty setup token",
			})
			return
		}

		// Retrieve the stored bcrypt hash and compare.
		storedHash, err := oidcConfigRepo.GetSetupTokenHash(c.Request.Context())
		if err != nil {
			slog.Error("TSM setup: failed to retrieve setup token hash", "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "failed to validate setup token",
			})
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(token)); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid setup token",
			})
			return
		}

		c.Next()
	}
}
