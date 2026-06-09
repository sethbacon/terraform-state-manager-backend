// auth.go implements the HTTP handlers for OIDC login, the OAuth callback, the
// current-user endpoint, token refresh, and logout. It mirrors the registry's
// auth flow, scoped to the OIDC (Keycloak) provider used for local development.
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
	"time"

	"github.com/gin-gonic/gin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
)

const sessionTTL = 24 * time.Hour

// AuthHandlers serves the authentication endpoints.
type AuthHandlers struct {
	cfg          *config.Config
	userRepo     *idstore.UserRepository
	orgRepo      *idstore.OrganizationRepository
	tokenRepo    *idstore.TokenRepository
	oidcProvider *auth.OIDCProvider
	stateStore   auth.StateStore
}

// NewAuthHandlers constructs the auth handlers. identityDB must resolve to the
// identity schema (search_path=identity,public). The OIDC provider is initialised
// only when OIDC is enabled in config.
func NewAuthHandlers(cfg *config.Config, identityDB *sql.DB) (*AuthHandlers, error) {
	h := &AuthHandlers{
		cfg:        cfg,
		userRepo:   idstore.NewUserRepository(identityDB),
		orgRepo:    idstore.NewOrganizationRepository(identityDB),
		tokenRepo:  idstore.NewTokenRepository(identityDB),
		stateStore: auth.NewMemoryStateStore(),
	}
	if cfg.Auth.OIDC.Enabled {
		p, err := auth.NewOIDCProvider(&cfg.Auth.OIDC)
		if err != nil {
			return nil, err
		}
		h.oidcProvider = p
	}
	return h, nil
}

// UserRepo and TokenRepo expose the identity repositories for the auth middleware.
func (h *AuthHandlers) UserRepo() *idstore.UserRepository   { return h.userRepo }
func (h *AuthHandlers) TokenRepo() *idstore.TokenRepository { return h.tokenRepo }

func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// cookieSecure reports whether cookies should carry the Secure attribute. It is
// derived from server configuration (direct TLS, or a configured https public
// URL) — never from the client-controlled X-Forwarded-Proto header, which an
// attacker could spoof to strip the Secure flag. Plain http://localhost
// development yields false so the auth cookie is still sent.
func (h *AuthHandlers) cookieSecure(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.HasPrefix(strings.ToLower(h.cfg.Server.PublicURL), "https://")
}

func (h *AuthHandlers) setSessionCookies(c *gin.Context, token string) {
	secure := h.cookieSecure(c)
	// #nosec G124 -- session cookie is HttpOnly; Secure is config-derived (true in
	// production, false only for http://localhost dev so the cookie is still sent).
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     middleware.AuthCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	if _, err := middleware.SetCSRFCookie(c.Writer, secure); err != nil {
		slog.Error("failed to set CSRF cookie", "error", err)
	}
}

func (h *AuthHandlers) clearSessionCookies(c *gin.Context) {
	// #nosec G124 -- expiring the HttpOnly session cookie on logout (MaxAge -1);
	// Secure is config-derived, matching how the cookie was originally set.
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     middleware.AuthCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   h.cookieSecure(c),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	middleware.ClearCSRFCookie(c.Writer)
}

// deriveFrontendURL returns the browser-facing base URL of the SPA.
func deriveFrontendURL(cfg *config.Config) string {
	if cfg.Server.PublicURL != "" {
		return strings.TrimRight(cfg.Server.PublicURL, "/")
	}
	if cfg.Auth.OIDC.RedirectURL != "" {
		if u, err := url.Parse(cfg.Auth.OIDC.RedirectURL); err == nil {
			return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
		}
	}
	return strings.TrimRight(cfg.Server.BaseURL, "/")
}

// ProvidersHandler lists configured auth providers for the login page.
func (h *AuthHandlers) ProvidersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		providers := make([]gin.H, 0, 1)
		if h.oidcProvider != nil {
			providers = append(providers, gin.H{"type": "oidc", "name": "OpenID Connect"})
		}
		c.JSON(http.StatusOK, gin.H{"providers": providers, "dev_mode": auth.IsDevMode()})
	}
}

// LoginHandler begins the OIDC authorization-code flow.
func (h *AuthHandlers) LoginHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.oidcProvider == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "OIDC provider not configured"})
			return
		}
		state, err := generateState()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate state"})
			return
		}
		ss := &auth.SessionState{State: state, CreatedAt: time.Now(), ProviderType: "oidc"}
		if err := h.stateStore.Save(c.Request.Context(), state, ss, 10*time.Minute); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save session state"})
			return
		}
		c.Redirect(http.StatusFound, h.oidcProvider.GetAuthURL(state))
	}
}

// CallbackHandler completes the OIDC flow: exchanges the code, provisions the
// user + membership, issues a JWT, sets the session cookie, and redirects to the
// frontend callback page.
func (h *AuthHandlers) CallbackHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		frontendBase := deriveFrontendURL(h.cfg)
		fail := func(code, desc string) {
			if frontendBase == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": desc})
				return
			}
			c.Redirect(http.StatusFound, fmt.Sprintf("%s/auth/callback?error=%s&error_description=%s",
				frontendBase, url.QueryEscape(code), url.QueryEscape(desc)))
		}

		if h.oidcProvider == nil {
			fail("provider_not_configured", "OIDC provider is not configured.")
			return
		}

		ctx := c.Request.Context()
		state := c.Query("state")
		ss, err := h.stateStore.Load(ctx, state)
		if err != nil || ss == nil {
			fail("invalid_state", "Invalid or expired login state. Please try again.")
			return
		}

		token, err := h.oidcProvider.ExchangeCode(ctx, c.Query("code"))
		if err != nil {
			fail("token_exchange_failed", "Failed to exchange the authorization code.")
			return
		}
		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok {
			fail("no_id_token", "The identity provider did not return an ID token.")
			return
		}
		idToken, err := h.oidcProvider.VerifyIDToken(ctx, rawIDToken)
		if err != nil {
			fail("id_token_invalid", "The ID token could not be verified.")
			return
		}
		sub, email, name, err := h.oidcProvider.ExtractUserInfo(idToken)
		if err != nil {
			fail("user_info_failed", "Failed to read user information from the ID token.")
			return
		}

		user, err := h.userRepo.GetOrCreateUserByOIDC(ctx, sub, email, name)
		if err != nil {
			fail("user_creation_failed", "Failed to look up or create your account.")
			return
		}

		h.ensureDefaultMembership(ctx, user.ID)

		scopes, err := h.orgRepo.GetUserCombinedScopes(ctx, user.ID)
		if err != nil {
			scopes = []string{}
		}

		jwtToken, err := auth.GenerateJWT(user.ID, user.Email, scopes, sessionTTL)
		if err != nil {
			fail("jwt_failed", "Failed to generate an authentication token.")
			return
		}

		h.setSessionCookies(c, jwtToken)
		c.Redirect(http.StatusFound, frontendBase+"/auth/callback")
	}
}

// MeHandler returns the authenticated user, memberships, and combined scopes.
func (h *AuthHandlers) MeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := c.Get("user_id")
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}
		uid, _ := userID.(string)

		userWithRoles, err := h.userRepo.GetUserWithOrgRoles(c.Request.Context(), uid)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user information"})
			return
		}
		if userWithRoles == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}

		scopes, err := h.orgRepo.GetUserCombinedScopes(c.Request.Context(), uid)
		if err != nil {
			scopes = []string{}
		}

		memberships := make([]gin.H, 0, len(userWithRoles.Memberships))
		for _, m := range userWithRoles.Memberships {
			memberships = append(memberships, gin.H{
				"organization_id":   m.OrganizationID,
				"organization_name": m.OrganizationName,
				"role_template_name": m.RoleTemplateName,
				"role_template_scopes": m.RoleTemplateScopes,
			})
		}

		resp := gin.H{
			"user": gin.H{
				"id":    userWithRoles.ID,
				"email": userWithRoles.Email,
				"name":  userWithRoles.Name,
			},
			"memberships":    memberships,
			"allowed_scopes": scopes,
		}
		if claimsVal, ok := c.Get("jwt_claims"); ok {
			if claims, ok := claimsVal.(*auth.Claims); ok && claims.ExpiresAt != nil {
				resp["session_expires_at"] = claims.ExpiresAt.Time
			}
		}
		c.JSON(http.StatusOK, resp)
	}
}

// RefreshHandler issues a new JWT with fresh scopes and revokes the old one.
func (h *AuthHandlers) RefreshHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := c.Get("user_id")
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}
		uid, _ := userID.(string)

		user, err := h.userRepo.GetUserByID(c.Request.Context(), uid)
		if err != nil || user == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not found"})
			return
		}
		scopes, err := h.orgRepo.GetUserCombinedScopes(c.Request.Context(), user.ID)
		if err != nil {
			scopes = []string{}
		}
		h.revokeCurrent(c)

		newToken, err := auth.GenerateJWT(user.ID, user.Email, scopes, sessionTTL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate new token"})
			return
		}
		h.setSessionCookies(c, newToken)
		// Cookie-only sessions: the JWT is delivered as an HttpOnly cookie and is
		// never returned in the body (so it cannot be read or persisted by JS).
		c.JSON(http.StatusOK, gin.H{"expires_in": int(sessionTTL.Seconds())})
	}
}

// LogoutHandler revokes the JWT, clears cookies, and redirects to the OIDC
// end-session endpoint when available so the IdP SSO session is also terminated.
func (h *AuthHandlers) LogoutHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		h.revokeCurrent(c)
		h.clearSessionCookies(c)

		postLogout := deriveFrontendURL(h.cfg) + "/"
		if h.oidcProvider != nil {
			if endSession := h.oidcProvider.GetEndSessionEndpoint(); endSession != "" {
				if u, err := url.Parse(endSession); err == nil {
					q := u.Query()
					q.Set("post_logout_redirect_uri", postLogout)
					q.Set("client_id", h.cfg.Auth.OIDC.ClientID)
					u.RawQuery = q.Encode()
					c.Redirect(http.StatusFound, u.String())
					return
				}
			}
		}
		c.Redirect(http.StatusFound, postLogout)
	}
}

func (h *AuthHandlers) revokeCurrent(c *gin.Context) {
	if v, ok := c.Get("jwt_claims"); ok {
		if claims, ok := v.(*auth.Claims); ok && claims.JTI != "" && claims.ExpiresAt != nil {
			_ = h.tokenRepo.RevokeToken(c.Request.Context(), claims.JTI, claims.UserID, claims.ExpiresAt.Time)
		}
	}
}

// ensureDefaultMembership assigns the configured default role to the user in the
// default organization on their FIRST login (when no membership exists yet), so a
// new user receives baseline scopes. It never alters an existing member's role —
// promotions/demotions are an explicit admin action, not a side effect of login.
// Group-based mapping from the IdP can replace this later.
func (h *AuthHandlers) ensureDefaultMembership(ctx context.Context, userID string) {
	if h.cfg.Auth.OIDC.DefaultRole != "" {
		h.assignRole(ctx, userID, h.cfg.Auth.OIDC.DefaultRole)
	}
}

// assignRole adds the user to the default organization with the given role
// template ONLY if they are not already a member. An existing member's role is
// left untouched, so logging in can never re-escalate (or downgrade) privileges.
func (h *AuthHandlers) assignRole(ctx context.Context, userID, role string) {
	org, err := h.orgRepo.GetDefaultOrganization(ctx)
	if err != nil || org == nil {
		slog.Warn("default organization not found; cannot assign role", "error", err)
		return
	}
	isMember, _, err := h.orgRepo.CheckMembership(ctx, org.ID, userID)
	if err != nil {
		return
	}
	if isMember {
		// First-login-only: never re-assign or escalate an existing member.
		return
	}
	if err := h.orgRepo.AddMemberWithParams(ctx, org.ID, userID, role); err != nil {
		slog.Warn("failed to add membership", "user_id", userID, "role", role, "error", err)
	}
}
