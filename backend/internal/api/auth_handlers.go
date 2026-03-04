// Package api provides HTTP handler implementations for the Terraform State Manager.
package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
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

// SessionState holds the state for an in-progress OIDC login flow.
type SessionState struct {
	State        string
	CreatedAt    time.Time
	RedirectURL  string
	ProviderType string
}

// AuthHandlers handles OIDC authentication endpoints.
type AuthHandlers struct {
	cfg            *config.Config
	db             *sql.DB
	userRepo       *repositories.UserRepository
	orgRepo        *repositories.OrganizationRepository
	oidcConfigRepo *repositories.OIDCConfigRepository
	oidcProvider   atomic.Pointer[oidc.OIDCProvider]
	sessionStore   map[string]*SessionState
	sessionMu      sync.RWMutex
}

// NewAuthHandlers creates a new AuthHandlers instance. If OIDC is enabled in
// the static configuration the provider is initialised eagerly so that login
// is available immediately after startup.
func NewAuthHandlers(cfg *config.Config, db *sql.DB, oidcConfigRepo *repositories.OIDCConfigRepository) *AuthHandlers {
	h := &AuthHandlers{
		cfg:            cfg,
		db:             db,
		userRepo:       repositories.NewUserRepository(db),
		orgRepo:        repositories.NewOrganizationRepository(db),
		oidcConfigRepo: oidcConfigRepo,
		sessionStore:   make(map[string]*SessionState),
	}

	if cfg.Auth.OIDC.Enabled {
		provider, err := oidc.NewOIDCProvider(&cfg.Auth.OIDC)
		if err != nil {
			slog.Warn("Failed to initialize OIDC provider from config", "error", err)
		} else {
			h.oidcProvider.Store(provider)
			slog.Info("OIDC provider initialized from config", "issuer", cfg.Auth.OIDC.IssuerURL)
		}
	}

	return h
}

// SetOIDCProvider atomically replaces the current OIDC provider. This is used
// by the setup wizard to activate a newly-configured provider at runtime
// without requiring a server restart.
func (h *AuthHandlers) SetOIDCProvider(provider *oidc.OIDCProvider) {
	h.oidcProvider.Store(provider)
}

// generateState creates a cryptographically random state parameter encoded as
// base64url without padding.
func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random state: %w", err)
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}

// LoginHandler redirects the user to the OIDC provider's authorization
// endpoint. An optional "redirect_url" query parameter is remembered so the
// frontend can restore the original page after login.
func (h *AuthHandlers) LoginHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		provider := h.oidcProvider.Load()
		if provider == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "OIDC provider not configured",
				"message": "OIDC authentication has not been configured. Please complete the setup wizard.",
			})
			return
		}

		state, err := generateState()
		if err != nil {
			slog.Error("Failed to generate OIDC state", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initiate login"})
			return
		}

		redirectURL := c.Query("redirect_url")

		h.sessionMu.Lock()
		h.sessionStore[state] = &SessionState{
			State:        state,
			CreatedAt:    time.Now(),
			RedirectURL:  redirectURL,
			ProviderType: "oidc",
		}
		h.sessionMu.Unlock()

		authURL := provider.GetAuthURL(state)
		c.Redirect(http.StatusFound, authURL)
	}
}

// CallbackHandler processes the OIDC callback after the user authenticates
// with the identity provider. It validates state, exchanges the authorization
// code for tokens, verifies the ID token, upserts the user record and finally
// redirects the browser to the frontend with a short-lived JWT.
func (h *AuthHandlers) CallbackHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		frontendURL := deriveFrontendURL(h.cfg)

		// Check for an error returned by the OIDC provider.
		if errParam := c.Query("error"); errParam != "" {
			desc := c.Query("error_description")
			slog.Warn("OIDC provider returned error", "error", errParam, "description", desc)
			redirectWithError(c, frontendURL, errParam, desc)
			return
		}

		code := c.Query("code")
		state := c.Query("state")
		if code == "" || state == "" {
			redirectWithError(c, frontendURL, "invalid_request", "Missing code or state parameter")
			return
		}

		// Validate and consume the state token (one-time use).
		h.sessionMu.Lock()
		session, exists := h.sessionStore[state]
		if exists {
			delete(h.sessionStore, state)
		}
		h.sessionMu.Unlock()

		if !exists {
			redirectWithError(c, frontendURL, "invalid_state", "Unknown or expired state parameter")
			return
		}

		// State tokens are valid for 5 minutes.
		if time.Since(session.CreatedAt) > 5*time.Minute {
			redirectWithError(c, frontendURL, "expired_state", "Login session has expired. Please try again.")
			return
		}

		provider := h.oidcProvider.Load()
		if provider == nil {
			redirectWithError(c, frontendURL, "server_error", "OIDC provider not available")
			return
		}

		ctx := c.Request.Context()

		// Exchange the authorization code for an OAuth2 token set.
		oauth2Token, err := provider.ExchangeCode(ctx, code)
		if err != nil {
			slog.Error("Failed to exchange authorization code", "error", err)
			redirectWithError(c, frontendURL, "token_exchange_failed", "Failed to exchange authorization code")
			return
		}

		// Extract and verify the ID token.
		rawIDToken, ok := oauth2Token.Extra("id_token").(string)
		if !ok || rawIDToken == "" {
			redirectWithError(c, frontendURL, "missing_id_token", "ID token not found in provider response")
			return
		}

		idToken, err := provider.VerifyIDToken(ctx, rawIDToken)
		if err != nil {
			slog.Error("Failed to verify ID token", "error", err)
			redirectWithError(c, frontendURL, "token_verification_failed", "Failed to verify ID token")
			return
		}

		// Extract user information from the verified ID token.
		sub, email, name, err := provider.ExtractUserInfo(idToken)
		if err != nil {
			slog.Error("Failed to extract user info from ID token", "error", err)
			redirectWithError(c, frontendURL, "invalid_user_info", err.Error())
			return
		}

		// Upsert the user record (find by OIDC sub, fall back to email, or create).
		user, err := h.getOrCreateUserByOIDC(ctx, sub, email, name)
		if err != nil {
			slog.Error("Failed to get or create user", "error", err, "email", email, "sub", sub)
			redirectWithError(c, frontendURL, "user_creation_failed", "Failed to process user account")
			return
		}

		if !user.IsActive {
			redirectWithError(c, frontendURL, "account_disabled", "Your account has been deactivated. Please contact an administrator.")
			return
		}

		// Collect the combined RBAC scopes across all organisation memberships.
		scopes, err := h.getUserCombinedScopes(ctx, user.ID)
		if err != nil {
			slog.Error("Failed to get user scopes", "error", err, "user_id", user.ID)
			redirectWithError(c, frontendURL, "scope_error", "Failed to determine user permissions")
			return
		}

		// Mint a JWT for the frontend.
		token, err := auth.GenerateJWT(user.ID, user.Email, scopes, 1*time.Hour)
		if err != nil {
			slog.Error("Failed to generate JWT", "error", err, "user_id", user.ID)
			redirectWithError(c, frontendURL, "jwt_error", "Failed to generate authentication token")
			return
		}

		slog.Info("OIDC login successful", "user_id", user.ID, "email", user.Email)

		redirectTarget := frontendURL + "/auth/callback?token=" + url.QueryEscape(token)
		if session.RedirectURL != "" {
			redirectTarget += "&redirect_url=" + url.QueryEscape(session.RedirectURL)
		}
		c.Redirect(http.StatusFound, redirectTarget)
	}
}

// LogoutHandler terminates the browser session. If the OIDC provider exposes
// an end_session_endpoint the user is redirected there first; otherwise the
// handler redirects straight to the frontend.
func (h *AuthHandlers) LogoutHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		frontendURL := deriveFrontendURL(h.cfg)

		provider := h.oidcProvider.Load()
		if provider != nil {
			endSessionURL := provider.GetEndSessionEndpoint()
			if endSessionURL != "" {
				params := url.Values{}
				params.Set("post_logout_redirect_uri", frontendURL)
				c.Redirect(http.StatusFound, endSessionURL+"?"+params.Encode())
				return
			}
		}

		c.Redirect(http.StatusFound, frontendURL)
	}
}

// RefreshHandler issues a fresh JWT with up-to-date scopes. The caller must
// already be authenticated (user_id set in the gin context by auth middleware).
func (h *AuthHandlers) RefreshHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := c.Get("user_id")
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}

		uid, ok := userID.(string)
		if !ok || uid == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user identity"})
			return
		}

		ctx := c.Request.Context()

		user, err := h.userRepo.GetUserByID(ctx, uid)
		if err != nil || user == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			return
		}

		if !user.IsActive {
			c.JSON(http.StatusForbidden, gin.H{"error": "account disabled"})
			return
		}

		scopes, err := h.getUserCombinedScopes(ctx, uid)
		if err != nil {
			slog.Error("Failed to refresh user scopes", "error", err, "user_id", uid)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to refresh permissions"})
			return
		}

		token, err := auth.GenerateJWT(user.ID, user.Email, scopes, 1*time.Hour)
		if err != nil {
			slog.Error("Failed to generate refreshed JWT", "error", err, "user_id", uid)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"token":      token,
			"expires_in": 3600,
		})
	}
}

// MeHandler returns the authenticated user's profile, organisation
// memberships, and allowed scopes.
func (h *AuthHandlers) MeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := c.Get("user_id")
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}

		uid, ok := userID.(string)
		if !ok || uid == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user identity"})
			return
		}

		ctx := c.Request.Context()

		user, err := h.userRepo.GetUserByID(ctx, uid)
		if err != nil || user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}

		memberships, err := h.getUserMemberships(ctx, uid)
		if err != nil {
			slog.Error("Failed to get user memberships", "error", err, "user_id", uid)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user memberships"})
			return
		}

		scopes, err := h.getUserCombinedScopes(ctx, uid)
		if err != nil {
			slog.Error("Failed to get user scopes for /me", "error", err, "user_id", uid)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load user permissions"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"id":             user.ID,
			"email":          user.Email,
			"name":           user.Name,
			"oidc_sub":       user.OIDCSub,
			"is_active":      user.IsActive,
			"created_at":     user.CreatedAt,
			"memberships":    memberships,
			"allowed_scopes": scopes,
		})
	}
}

// ---------------------------------------------------------------------------
// Private helpers
// ---------------------------------------------------------------------------

// getOrCreateUserByOIDC looks up a user by OIDC subject, falls back to an
// email match (and links the OIDC subject), or creates a brand-new account.
func (h *AuthHandlers) getOrCreateUserByOIDC(ctx context.Context, sub, email, name string) (*models.User, error) {
	// Primary lookup: OIDC subject identifier.
	user, err := h.userRepo.GetUserByOIDCSub(ctx, sub)
	if err != nil {
		return nil, fmt.Errorf("lookup by OIDC sub: %w", err)
	}
	if user != nil {
		// Synchronise profile fields that may have changed at the IdP.
		if user.Email != email || user.Name != name {
			user.Email = email
			user.Name = name
			if updateErr := h.userRepo.UpdateUser(ctx, user); updateErr != nil {
				slog.Warn("Failed to update user profile after OIDC login",
					"user_id", user.ID, "error", updateErr)
			}
		}
		return user, nil
	}

	// Fallback: match by email and link the OIDC subject.
	user, err = h.userRepo.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("lookup by email: %w", err)
	}
	if user != nil {
		user.OIDCSub = &sub
		user.Name = name
		if err := h.userRepo.UpdateUser(ctx, user); err != nil {
			return nil, fmt.Errorf("link OIDC sub to existing user: %w", err)
		}
		return user, nil
	}

	// No existing account -- create a new one.
	newUser := &models.User{
		Email:    email,
		Name:     name,
		OIDCSub:  &sub,
		IsActive: true,
	}
	if err := h.userRepo.CreateUser(ctx, newUser); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	// Automatically add the new user to the default organisation so they
	// receive baseline RBAC scopes.
	h.addUserToDefaultOrg(ctx, newUser.ID)

	return newUser, nil
}

// addUserToDefaultOrg adds a user to the default organisation with the
// configured default role template. Failures are logged but do not propagate
// so that the login flow is not interrupted.
func (h *AuthHandlers) addUserToDefaultOrg(ctx context.Context, userID string) {
	defaultOrgName := h.cfg.MultiTenancy.DefaultOrganization
	if defaultOrgName == "" {
		defaultOrgName = "default"
	}

	org, err := h.orgRepo.GetOrganizationByName(ctx, defaultOrgName)
	if err != nil || org == nil {
		slog.Warn("Default organisation not found for new OIDC user",
			"org_name", defaultOrgName, "user_id", userID)
		return
	}

	// Resolve the default role template (if configured).
	var roleTemplateID *string
	if h.cfg.Auth.OIDC.DefaultRole != "" {
		var rtID string
		err := h.db.QueryRowContext(ctx,
			"SELECT id FROM role_templates WHERE name = $1",
			h.cfg.Auth.OIDC.DefaultRole,
		).Scan(&rtID)
		if err == nil {
			roleTemplateID = &rtID
		} else {
			slog.Warn("Default OIDC role template not found",
				"role", h.cfg.Auth.OIDC.DefaultRole, "error", err)
		}
	}

	member := &models.OrganizationMember{
		OrganizationID: org.ID,
		UserID:         userID,
		RoleTemplateID: roleTemplateID,
	}
	if err := h.orgRepo.AddMember(ctx, member); err != nil {
		slog.Warn("Failed to add OIDC user to default organisation",
			"user_id", userID, "org_id", org.ID, "error", err)
	}
}

// getUserCombinedScopes aggregates all distinct RBAC scopes that the user has
// been granted across every organisation membership.
func (h *AuthHandlers) getUserCombinedScopes(ctx context.Context, userID string) ([]string, error) {
	var scopeCSV sql.NullString
	err := h.db.QueryRowContext(ctx,
		`SELECT string_agg(DISTINCT scope_val, ',')
		 FROM organization_members om
		 JOIN role_templates rt ON om.role_template_id = rt.id
		 CROSS JOIN LATERAL unnest(rt.scopes) AS scope_val
		 WHERE om.user_id = $1`,
		userID,
	).Scan(&scopeCSV)
	if err != nil {
		// No memberships or role templates -- return empty scopes.
		return []string{}, nil
	}

	if !scopeCSV.Valid || scopeCSV.String == "" {
		return []string{}, nil
	}

	parts := strings.Split(scopeCSV.String, ",")
	scopes := make([]string, 0, len(parts))
	for _, s := range parts {
		s = strings.TrimSpace(s)
		if s != "" {
			scopes = append(scopes, s)
		}
	}
	return scopes, nil
}

// membershipInfo is a lightweight projection of an organisation membership
// used by the /me endpoint.
type membershipInfo struct {
	OrganizationID   string  `json:"organization_id"`
	OrganizationName string  `json:"organization_name"`
	DisplayName      string  `json:"display_name"`
	RoleName         *string `json:"role_name,omitempty"`
}

// getUserMemberships returns all organisation memberships for a user.
func (h *AuthHandlers) getUserMemberships(ctx context.Context, userID string) ([]membershipInfo, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT o.id, o.name, o.display_name, rt.name
		 FROM organization_members om
		 JOIN organizations o ON om.organization_id = o.id
		 LEFT JOIN role_templates rt ON om.role_template_id = rt.id
		 WHERE om.user_id = $1
		 ORDER BY o.name`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("query user memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	memberships := make([]membershipInfo, 0)
	for rows.Next() {
		var m membershipInfo
		if err := rows.Scan(&m.OrganizationID, &m.OrganizationName, &m.DisplayName, &m.RoleName); err != nil {
			return nil, fmt.Errorf("scan membership row: %w", err)
		}
		memberships = append(memberships, m)
	}
	return memberships, nil
}

// redirectWithError sends the browser to the frontend's /auth/callback page
// with error and error_description query parameters.
func redirectWithError(c *gin.Context, frontendURL, errCode, errDescription string) {
	target := fmt.Sprintf("%s/auth/callback?error=%s&error_description=%s",
		frontendURL,
		url.QueryEscape(errCode),
		url.QueryEscape(errDescription),
	)
	c.Redirect(http.StatusFound, target)
}

// deriveFrontendURL determines the public-facing frontend URL from the
// configuration. It tries, in order: Server.PublicURL, the origin of the OIDC
// RedirectURL, and finally Server.BaseURL.
func deriveFrontendURL(cfg *config.Config) string {
	if cfg.Server.PublicURL != "" {
		return strings.TrimRight(cfg.Server.PublicURL, "/")
	}

	if cfg.Auth.OIDC.RedirectURL != "" {
		if u, err := url.Parse(cfg.Auth.OIDC.RedirectURL); err == nil && u.Host != "" {
			origin := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
			return strings.TrimRight(origin, "/")
		}
	}

	return strings.TrimRight(cfg.Server.BaseURL, "/")
}
