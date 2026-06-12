package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// AuthCookieName is the HttpOnly cookie carrying the session JWT.
const AuthCookieName = "tsm_auth_token"

// APIKeyPrefix is the fixed prefix of TSM API keys (registry pattern: the
// first 10 characters of the full key are stored as the indexed lookup prefix).
const APIKeyPrefix = "tsm"

// AuthMiddleware validates the session JWT (from the Authorization header or the
// auth cookie), checks revocation, loads the user, and populates the request
// context with user_id, scopes (from claims), and jwt_claims. Header tokens that
// are not JWTs fall through to API-key authentication (registry order: JWT is
// stateless and tried first; keys cost a prefix lookup + bcrypt compare).
func AuthMiddleware(userRepo *idstore.UserRepository, tokenRepo *idstore.TokenRepository, apiKeyRepo *idstore.APIKeyRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		// A verified mTLS client certificate (set by mtls.AuthMiddleware earlier in
		// the chain) already authenticated this request and populated scopes.
		if m, ok := c.Get("auth_method"); ok && m == "mtls" {
			c.Next()
			return
		}

		token, fromCookie := extractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Missing authorization credentials"})
			return
		}

		claims, err := auth.ValidateJWT(token)
		if err != nil {
			// API keys arrive only via the Authorization header (never cookies).
			if !fromCookie && apiKeyRepo != nil && strings.HasPrefix(token, APIKeyPrefix+"_") &&
				authenticateAPIKey(c, apiKeyRepo, userRepo, token) {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid credentials"})
			return
		}

		if claims.JTI != "" && tokenRepo != nil {
			revoked, rErr := tokenRepo.IsTokenRevoked(c.Request.Context(), claims.JTI)
			if rErr != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Auth check failed"})
				return
			}
			if revoked {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Token has been revoked"})
				return
			}
		}

		user, err := userRepo.GetUserByID(c.Request.Context(), claims.UserID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to load user"})
			return
		}
		if user == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "User not found"})
			return
		}

		setAuthContext(c, user.ID, claims, fromCookie)
		c.Next()
	}
}

// OptionalAuthMiddleware populates the auth context when a valid session token is
// present but never aborts. Used for endpoints like logout that are idempotent
// and must work even without (or with an expired) session.
func OptionalAuthMiddleware(userRepo *idstore.UserRepository, tokenRepo *idstore.TokenRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, fromCookie := extractToken(c)
		if token == "" {
			c.Next()
			return
		}
		claims, err := auth.ValidateJWT(token)
		if err != nil {
			c.Next()
			return
		}
		if claims.JTI != "" && tokenRepo != nil {
			if revoked, rErr := tokenRepo.IsTokenRevoked(c.Request.Context(), claims.JTI); rErr == nil && revoked {
				c.Next()
				return
			}
		}
		if user, err := userRepo.GetUserByID(c.Request.Context(), claims.UserID); err == nil && user != nil {
			setAuthContext(c, user.ID, claims, fromCookie)
		}
		c.Next()
	}
}

// authenticateAPIKey resolves a Bearer token as an API key: indexed prefix
// lookup → bcrypt compare → expiry check → owning user must still exist →
// context populated with the key's static scopes. Last-used is recorded
// async so the request never blocks on the bookkeeping write.
func authenticateAPIKey(c *gin.Context, keys *idstore.APIKeyRepository, users *idstore.UserRepository, token string) bool {
	if len(token) < idauth.DisplayPrefixLength {
		return false
	}
	ctx := c.Request.Context()
	candidates, err := keys.GetAPIKeysByPrefix(ctx, token[:idauth.DisplayPrefixLength])
	if err != nil {
		return false
	}
	for _, k := range candidates {
		if !idauth.ValidateAPIKey(token, k.KeyHash) {
			continue
		}
		// The bcrypt hash matched: this IS the presented key. Expired or
		// orphaned keys deny outright (no fall-through to other candidates).
		if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
			return false
		}
		userID := ""
		if k.UserID != nil {
			userID = *k.UserID
		}
		if userID != "" && users != nil {
			user, uErr := users.GetUserByID(ctx, userID)
			if uErr != nil || user == nil {
				return false
			}
		}
		scopes := k.Scopes
		if scopes == nil {
			scopes = []string{}
		}
		c.Set("user_id", userID)
		c.Set("scopes", scopes)
		c.Set("auth_method", "apikey")
		c.Set("api_key_id", k.ID)
		go func(id string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = keys.UpdateLastUsed(ctx, id)
		}(k.ID)
		return true
	}
	return false
}

func extractToken(c *gin.Context) (token string, fromCookie bool) {
	if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer ")), false
	}
	if v, err := c.Cookie(AuthCookieName); err == nil && v != "" {
		return v, true
	}
	return "", false
}

func setAuthContext(c *gin.Context, userID string, claims *auth.Claims, fromCookie bool) {
	c.Set("user_id", userID)
	c.Set("jwt_claims", claims)
	if fromCookie {
		c.Set("auth_method", "jwt_cookie")
	} else {
		c.Set("auth_method", "jwt")
	}
	scopes := claims.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	c.Set("scopes", scopes)
}
