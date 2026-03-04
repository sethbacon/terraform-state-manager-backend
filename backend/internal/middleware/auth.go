package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// AuthMiddleware returns a gin.HandlerFunc that authenticates requests using
// either a JWT token or an API key. JWT authentication is tried first
// (stateless); if the Authorization header does not carry a valid JWT the
// middleware falls back to API-key authentication (prefix lookup + bcrypt
// comparison).
//
// On success the following values are stored in the gin context:
//   - "user"        *models.User
//   - "user_id"     string
//   - "auth_method" string ("jwt" | "api_key")
//   - "scopes"      []string
//
// On failure the handler is aborted with a 401 response.
func AuthMiddleware(
	cfg *config.Config,
	userRepo *repositories.UserRepository,
	apiKeyRepo *repositories.APIKeyRepository,
	orgRepo *repositories.OrganizationRepository,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "authorization header is required",
			})
			return
		}

		// --- Try JWT first ---
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
			tokenStr = strings.TrimSpace(tokenStr)

			// JWT tokens are not prefixed with "tsm_" (API keys are).
			if tokenStr != "" && !strings.HasPrefix(tokenStr, cfg.Auth.APIKeys.Prefix+"_") {
				claims, err := auth.ValidateJWT(tokenStr)
				if err == nil {
					// Resolve the user from the database.
					user, err := userRepo.GetUserByID(c.Request.Context(), claims.UserID)
					if err != nil || user == nil {
						slog.Warn("TSM auth: JWT user not found",
							"user_id", claims.UserID,
							"error", err,
						)
						c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
							"error": "user not found",
						})
						return
					}

					c.Set("user", user)
					c.Set("user_id", claims.UserID)
					c.Set("email", claims.Email)
					c.Set("auth_method", "jwt")
					c.Set("scopes", claims.Scopes)

					// Resolve the user's organization so handlers can scope
					// queries to the correct org without an extra DB lookup.
					userWithOrg, orgErr := userRepo.GetUserWithOrgRoles(c.Request.Context(), claims.UserID)
					if orgErr == nil && userWithOrg != nil && userWithOrg.OrganizationID != nil {
						c.Set("organization_id", *userWithOrg.OrganizationID)
					}

					c.Next()
					return
				}
				// If JWT validation failed, fall through to API-key attempt.
			}
		}

		// --- Try API-key authentication ---
		apiKey := ""
		if strings.HasPrefix(authHeader, "Bearer ") {
			apiKey = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		} else {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid authorization format",
			})
			return
		}

		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "empty credentials",
			})
			return
		}

		authenticated, foundKey := authenticateAPIKey(c.Request.Context(), apiKey, apiKeyRepo)
		if !authenticated || foundKey == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid API key",
			})
			return
		}

		// Resolve the user if the key is associated with one.
		var user *models.User
		if foundKey.UserID != nil && *foundKey.UserID != "" {
			u, err := userRepo.GetUserByID(c.Request.Context(), *foundKey.UserID)
			if err != nil || u == nil {
				slog.Warn("TSM auth: API key user not found",
					"api_key_id", foundKey.ID,
					"user_id", *foundKey.UserID,
					"error", err,
				)
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"error": "API key user not found",
				})
				return
			}
			user = u
		}

		if user != nil {
			c.Set("user", user)
			if foundKey.UserID != nil {
				c.Set("user_id", *foundKey.UserID)
			}
		}
		c.Set("api_key_id", foundKey.ID)
		c.Set("auth_method", "api_key")
		c.Set("scopes", foundKey.Scopes)
		if foundKey.OrganizationID != "" {
			c.Set("org_id", foundKey.OrganizationID)
			c.Set("organization_id", foundKey.OrganizationID)
		}

		// Update LastUsed asynchronously so authentication latency is not
		// affected by the database write.
		go func(keyID string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := apiKeyRepo.UpdateLastUsed(ctx, keyID); err != nil {
				slog.Error("TSM auth: failed to update API key last used",
					"api_key_id", keyID,
					"error", err,
				)
			}
		}(foundKey.ID)

		c.Next()
	}
}

// OptionalAuthMiddleware behaves like AuthMiddleware but does NOT abort the
// request when no valid credentials are provided. It simply continues to the
// next handler, leaving the context without authentication values.
func OptionalAuthMiddleware(
	cfg *config.Config,
	userRepo *repositories.UserRepository,
	apiKeyRepo *repositories.APIKeyRepository,
	orgRepo *repositories.OrganizationRepository,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Next()
			return
		}

		// --- Try JWT first ---
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
			tokenStr = strings.TrimSpace(tokenStr)

			if tokenStr != "" && !strings.HasPrefix(tokenStr, cfg.Auth.APIKeys.Prefix+"_") {
				claims, err := auth.ValidateJWT(tokenStr)
				if err == nil {
					user, err := userRepo.GetUserByID(c.Request.Context(), claims.UserID)
					if err == nil {
						c.Set("user", user)
						c.Set("user_id", claims.UserID)
						c.Set("auth_method", "jwt")
						c.Set("scopes", claims.Scopes)

						userWithOrg, orgErr := userRepo.GetUserWithOrgRoles(c.Request.Context(), claims.UserID)
						if orgErr == nil && userWithOrg != nil && userWithOrg.OrganizationID != nil {
							c.Set("organization_id", *userWithOrg.OrganizationID)
						}
					}
					c.Next()
					return
				}
			}
		}

		// --- Try API-key authentication ---
		if strings.HasPrefix(authHeader, "Bearer ") {
			apiKey := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			if apiKey != "" {
				authenticated, foundKey := authenticateAPIKey(c.Request.Context(), apiKey, apiKeyRepo)
				if authenticated && foundKey != nil {
					if foundKey.UserID != nil && *foundKey.UserID != "" {
						user, err := userRepo.GetUserByID(c.Request.Context(), *foundKey.UserID)
						if err == nil {
							c.Set("user", user)
							c.Set("user_id", *foundKey.UserID)
						}
					}
					c.Set("api_key_id", foundKey.ID)
					c.Set("auth_method", "api_key")
					c.Set("scopes", foundKey.Scopes)
					if foundKey.OrganizationID != "" {
						c.Set("org_id", foundKey.OrganizationID)
					}

					go func(keyID string) {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := apiKeyRepo.UpdateLastUsed(ctx, keyID); err != nil {
							slog.Error("TSM auth: failed to update API key last used",
								"api_key_id", keyID,
								"error", err,
							)
						}
					}(foundKey.ID)
				}
			}
		}

		c.Next()
	}
}

// authenticateAPIKey extracts the prefix from the provided key, looks up
// candidate keys by prefix, and compares each using bcrypt. It returns
// true together with the matching models.APIKey on success.
func authenticateAPIKey(
	ctx context.Context,
	rawKey string,
	apiKeyRepo *repositories.APIKeyRepository,
) (bool, *models.APIKey) {
	// API keys are formatted as "<prefix>_<random>". Extract the lookup
	// prefix (first 10 chars or up to the underscore).
	prefix := rawKey
	if len(prefix) > auth.DisplayPrefixLength {
		prefix = prefix[:auth.DisplayPrefixLength]
	}

	candidates, err := apiKeyRepo.GetAPIKeysByPrefix(ctx, prefix)
	if err != nil {
		slog.Error("TSM auth: failed to fetch API keys by prefix",
			"prefix", prefix,
			"error", err,
		)
		return false, nil
	}

	for _, candidate := range candidates {
		// Skip expired keys.
		if candidate.ExpiresAt != nil && candidate.ExpiresAt.Before(time.Now()) {
			continue
		}

		if auth.ValidateAPIKey(rawKey, candidate.KeyHash) {
			return true, candidate
		}
	}

	return false, nil
}
