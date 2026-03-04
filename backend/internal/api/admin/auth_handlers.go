// auth_handlers.go implements OIDC login/callback, JWT refresh, logout, and
// current-user (/me) endpoints for the Terraform State Manager.
package admin

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/oidc"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// sessionState holds in-memory OIDC login state for CSRF protection.
type sessionState struct {
	State     string
	CreatedAt time.Time
}

// AuthHandlers provides HTTP handlers for authentication endpoints.
type AuthHandlers struct {
	cfg            *config.Config
	db             *sql.DB
	userRepo       *repositories.UserRepository
	orgRepo        *repositories.OrganizationRepository
	oidcConfigRepo *repositories.OIDCConfigRepository

	oidcProvider atomic.Pointer[oidc.OIDCProvider]

	mu       sync.RWMutex
	sessions map[string]*sessionState // keyed by state parameter
}

// NewAuthHandlers creates a new AuthHandlers instance.
func NewAuthHandlers(cfg *config.Config, db *sql.DB, oidcConfigRepo *repositories.OIDCConfigRepository) (*AuthHandlers, error) {
	h := &AuthHandlers{
		cfg:            cfg,
		db:             db,
		userRepo:       repositories.NewUserRepository(db),
		orgRepo:        repositories.NewOrganizationRepository(db),
		oidcConfigRepo: oidcConfigRepo,
		sessions:       make(map[string]*sessionState),
	}

	// Try to initialise OIDC provider from static config.
	if cfg.Auth.OIDC.Enabled {
		provider, err := oidc.NewOIDCProvider(&cfg.Auth.OIDC)
		if err != nil {
			slog.Warn("OIDC provider not available from static config, will rely on database config", "error", err)
		} else {
			h.oidcProvider.Store(provider)
		}
	}

	// Background goroutine to prune expired session states every 5 minutes.
	go h.pruneExpiredSessions()

	return h, nil
}

// SetOIDCProvider atomically swaps the live OIDC provider (used by the setup
// wizard after OIDC configuration is saved to the database).
func (h *AuthHandlers) SetOIDCProvider(provider *oidc.OIDCProvider) {
	h.oidcProvider.Store(provider)
	slog.Info("OIDC provider updated at runtime")
}

// generateState returns a 32-byte, base64url-encoded random string.
func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pruneExpiredSessions removes session states older than 10 minutes.
func (h *AuthHandlers) pruneExpiredSessions() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		for k, s := range h.sessions {
			if time.Since(s.CreatedAt) > 10*time.Minute {
				delete(h.sessions, k)
			}
		}
		h.mu.Unlock()
	}
}

// LoginHandler initiates the OIDC authorization code flow.
// GET /api/v1/auth/login
func (h *AuthHandlers) LoginHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		provider := h.oidcProvider.Load()
		if provider == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "OIDC authentication is not configured. Complete the setup wizard first.",
			})
			return
		}

		state, err := generateState()
		if err != nil {
			slog.Error("failed to generate OIDC state", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to initiate login"})
			return
		}

		h.mu.Lock()
		h.sessions[state] = &sessionState{State: state, CreatedAt: time.Now()}
		h.mu.Unlock()

		authURL := provider.GetAuthURL(state)
		c.Redirect(http.StatusTemporaryRedirect, authURL)
	}
}

// CallbackHandler handles the OIDC authorization code callback.
// GET /api/v1/auth/callback
func (h *AuthHandlers) CallbackHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		provider := h.oidcProvider.Load()
		if provider == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "OIDC not configured"})
			return
		}

		// Validate state parameter
		state := c.Query("state")
		h.mu.RLock()
		sess, ok := h.sessions[state]
		h.mu.RUnlock()

		if !ok || state == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid or missing state parameter"})
			return
		}

		if time.Since(sess.CreatedAt) > 5*time.Minute {
			h.mu.Lock()
			delete(h.sessions, state)
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "Login session expired, please try again"})
			return
		}

		// Clean up used state
		h.mu.Lock()
		delete(h.sessions, state)
		h.mu.Unlock()

		// Check for OIDC error response
		if errParam := c.Query("error"); errParam != "" {
			desc := c.Query("error_description")
			slog.Warn("OIDC callback received error", "error", errParam, "description", desc)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Authentication failed: " + errParam})
			return
		}

		code := c.Query("code")
		if code == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Missing authorization code"})
			return
		}

		// Exchange authorization code for tokens
		oauth2Token, err := provider.ExchangeCode(c.Request.Context(), code)
		if err != nil {
			slog.Error("failed to exchange authorization code", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to exchange authorization code"})
			return
		}

		// Extract and verify ID token
		rawIDToken, ok := oauth2Token.Extra("id_token").(string)
		if !ok || rawIDToken == "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "No ID token in response"})
			return
		}

		idToken, err := provider.VerifyIDToken(c.Request.Context(), rawIDToken)
		if err != nil {
			slog.Error("failed to verify ID token", "error", err)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Failed to verify ID token"})
			return
		}

		// Extract user info from ID token
		sub, email, name, err := provider.ExtractUserInfo(idToken)
		if err != nil {
			slog.Error("failed to extract user info from ID token", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to extract user information"})
			return
		}

		// Find or create user
		user, err := h.findOrCreateUser(c, sub, email, name)
		if err != nil {
			slog.Error("failed to find or create user", "error", err, "email", email)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process user account"})
			return
		}

		if !user.IsActive {
			c.JSON(http.StatusForbidden, gin.H{"error": "User account is disabled"})
			return
		}

		// Resolve user scopes from organization membership
		scopes := h.resolveUserScopes(c, user.ID)

		// Generate JWT
		token, err := auth.GenerateJWT(user.ID, user.Email, scopes, 1*time.Hour)
		if err != nil {
			slog.Error("failed to generate JWT", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate authentication token"})
			return
		}

		// Redirect to frontend with token
		frontendURL := h.cfg.Server.GetPublicURL() + "/callback?token=" + token
		c.Redirect(http.StatusTemporaryRedirect, frontendURL)
	}
}

// findOrCreateUser looks up a user by OIDC subject or email, creating a new
// user record if none exists.
func (h *AuthHandlers) findOrCreateUser(c *gin.Context, sub, email, name string) (*models.User, error) {
	ctx := c.Request.Context()

	// Try lookup by OIDC subject first
	user, err := h.userRepo.GetUserByOIDCSub(ctx, sub)
	if err != nil {
		return nil, err
	}
	if user != nil {
		return user, nil
	}

	// Try lookup by email (user may exist from admin creation before OIDC login)
	user, err = h.userRepo.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if user != nil {
		// Link OIDC subject to existing user
		user.OIDCSub = &sub
		user.Name = name
		if err := h.userRepo.UpdateUser(ctx, user); err != nil {
			slog.Warn("failed to link OIDC sub to existing user", "error", err, "email", email)
		}
		return user, nil
	}

	// Create new user
	newUser := &models.User{
		Email:    email,
		Name:     name,
		OIDCSub:  &sub,
		IsActive: true,
	}
	if err := h.userRepo.CreateUser(ctx, newUser); err != nil {
		return nil, err
	}

	slog.Info("created new user from OIDC login", "email", email, "oidc_sub", sub)

	// Auto-assign to default organization if multi-tenancy is enabled
	if h.cfg.MultiTenancy.Enabled && h.cfg.MultiTenancy.DefaultOrganization != "" {
		org, err := h.orgRepo.GetOrganizationByName(ctx, h.cfg.MultiTenancy.DefaultOrganization)
		if err == nil && org != nil {
			member := &models.OrganizationMember{
				OrganizationID: org.ID,
				UserID:         newUser.ID,
			}
			if err := h.orgRepo.AddMember(ctx, member); err != nil {
				slog.Warn("failed to add new user to default org", "error", err, "org", org.Name)
			}
		}
	}

	return newUser, nil
}

// resolveUserScopes resolves the effective scopes for a user based on their
// organization memberships and role templates.
func (h *AuthHandlers) resolveUserScopes(c *gin.Context, userID string) []string {
	ctx := c.Request.Context()

	userWithRoles, err := h.userRepo.GetUserWithOrgRoles(ctx, userID)
	if err != nil || userWithRoles == nil {
		return auth.GetDefaultScopes()
	}

	scopes := userWithRoles.GetAllowedScopes()
	if len(scopes) == 0 {
		return auth.GetDefaultScopes()
	}

	return scopes
}

// LogoutHandler handles user logout.
// GET /api/v1/auth/logout
func (h *AuthHandlers) LogoutHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		// For OIDC, we can optionally redirect to the IdP's end-session endpoint
		provider := h.oidcProvider.Load()
		if provider != nil {
			endSessionURL := provider.GetEndSessionEndpoint()
			if endSessionURL != "" {
				c.JSON(http.StatusOK, gin.H{
					"message":          "Logged out successfully",
					"end_session_url":  endSessionURL,
					"post_logout_hint": h.cfg.Server.GetPublicURL(),
				})
				return
			}
		}

		c.JSON(http.StatusOK, gin.H{"message": "Logged out successfully"})
	}
}

// RefreshHandler issues a new JWT with an extended expiry.
// POST /api/v1/auth/refresh
func (h *AuthHandlers) RefreshHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, _ := c.Get("user_id")
		email, _ := c.Get("email")
		scopesRaw, _ := c.Get("scopes")

		userIDStr, _ := userID.(string)
		emailStr, _ := email.(string)

		var scopes []string
		if s, ok := scopesRaw.([]string); ok {
			scopes = s
		}

		if userIDStr == "" || emailStr == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid session"})
			return
		}

		// Re-resolve scopes from database to pick up any role changes
		freshScopes := h.resolveUserScopes(c, userIDStr)
		if len(freshScopes) > 0 {
			scopes = freshScopes
		}

		token, err := auth.GenerateJWT(userIDStr, emailStr, scopes, 1*time.Hour)
		if err != nil {
			slog.Error("failed to refresh JWT", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to refresh token"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"token": token})
	}
}

// MeHandler returns the current authenticated user's profile and scopes.
// GET /api/v1/auth/me
func (h *AuthHandlers) MeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, _ := c.Get("user_id")
		userIDStr, _ := userID.(string)

		if userIDStr == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		user, err := h.userRepo.GetUserByID(c.Request.Context(), userIDStr)
		if err != nil || user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}

		userWithRoles, err := h.userRepo.GetUserWithOrgRoles(c.Request.Context(), userIDStr)
		if err != nil {
			slog.Warn("failed to load user org roles for /me", "error", err)
		}

		response := gin.H{
			"id":        user.ID,
			"email":     user.Email,
			"name":      user.Name,
			"is_active": user.IsActive,
		}

		if userWithRoles != nil {
			response["organization_id"] = userWithRoles.OrganizationID
			response["organization_name"] = userWithRoles.OrganizationName
			response["role"] = userWithRoles.RoleTemplateName
			response["scopes"] = userWithRoles.Scopes
		}

		c.JSON(http.StatusOK, response)
	}
}
