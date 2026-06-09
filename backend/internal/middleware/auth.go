package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
)

// AuthCookieName is the HttpOnly cookie carrying the session JWT.
const AuthCookieName = "tsm_auth_token"

// AuthMiddleware validates the session JWT (from the Authorization header or the
// auth cookie), checks revocation, loads the user, and populates the request
// context with user_id, scopes (from claims), and jwt_claims.
func AuthMiddleware(userRepo *idstore.UserRepository, tokenRepo *idstore.TokenRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, fromCookie := extractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Missing authorization credentials"})
			return
		}

		claims, err := auth.ValidateJWT(token)
		if err != nil {
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
