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
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	idoidc "github.com/sethbacon/terraform-suite-identity/identity/auth/oidc"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/ldap"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/saml"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
)

const sessionTTL = 24 * time.Hour

// AuthHandlers serves the authentication endpoints.
type AuthHandlers struct {
	cfg           *config.Config
	userRepo      *idstore.UserRepository
	orgRepo       *idstore.OrganizationRepository
	tokenRepo     *idstore.TokenRepository
	apiKeyRepo    *idstore.APIKeyRepository
	oidcProvider  atomic.Pointer[auth.OIDCProvider]
	ldapProvider  *ldap.Provider
	samlProviders map[string]*saml.Provider // keyed by IdP name; nil when SAML disabled
	stateStore    auth.StateStore
	// ssoSettings reads the admin-editable OIDC group-mapping overlay. The table
	// lives in the app schema; the identity connection's search_path resolves it.
	ssoSettings *repositories.SSOSettingsRepository
	audit       auditor
}

// NewAuthHandlers constructs the auth handlers. identityDB must resolve to the
// identity schema (search_path=identity,public). The OIDC provider is initialised
// only when OIDC is enabled in config.
func NewAuthHandlers(cfg *config.Config, identityDB *sql.DB) (*AuthHandlers, error) {
	h := &AuthHandlers{
		cfg:         cfg,
		userRepo:    idstore.NewUserRepository(identityDB),
		orgRepo:     idstore.NewOrganizationRepository(identityDB),
		tokenRepo:   idstore.NewTokenRepository(identityDB),
		apiKeyRepo:  idstore.NewAPIKeyRepository(identityDB),
		stateStore:  auth.NewMemoryStateStore(),
		ssoSettings: repositories.NewSSOSettingsRepository(identityDB),
		audit:       newAuditor(identityDB),
	}
	if cfg.Auth.OIDC.Enabled {
		p, err := auth.NewOIDCProvider(&cfg.Auth.OIDC)
		if err != nil {
			return nil, err
		}
		h.oidcProvider.Store(p)
	}
	if cfg.Auth.LDAP.Enabled {
		p, err := ldap.NewProvider(cfg.Auth.LDAP)
		if err != nil {
			return nil, err
		}
		h.ldapProvider = p
	}
	if cfg.Auth.SAML.Enabled {
		h.samlProviders = make(map[string]*saml.Provider, len(cfg.Auth.SAML.IdPs))
		for _, idp := range cfg.Auth.SAML.IdPs {
			p, err := saml.NewProvider(cfg.Auth.SAML, idp)
			if err != nil {
				return nil, fmt.Errorf("saml idp %q: %w", idp.Name, err)
			}
			h.samlProviders[idp.Name] = p
			slog.Info("SAML IdP configured", "name", idp.Name)
		}
	}
	return h, nil
}

// UserRepo and TokenRepo expose the identity repositories for the auth middleware.
func (h *AuthHandlers) UserRepo() *idstore.UserRepository        { return h.userRepo }
func (h *AuthHandlers) TokenRepo() *idstore.TokenRepository      { return h.tokenRepo }
func (h *AuthHandlers) APIKeyRepo() *idstore.APIKeyRepository    { return h.apiKeyRepo }
func (h *AuthHandlers) OrgRepo() *idstore.OrganizationRepository { return h.orgRepo }

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

// SetOIDCProvider atomically swaps the active OIDC provider. The setup wizard
// calls this to activate a freshly configured provider at runtime — no restart —
// and the boot path calls it to load a DB-stored config. Login/Callback read the
// provider via Load(), so the swap is race-free.
func (h *AuthHandlers) SetOIDCProvider(p *auth.OIDCProvider) {
	h.oidcProvider.Store(p)
	slog.Info("OIDC provider activated at runtime")
}

// ProvidersHandler lists configured auth providers for the login page picker.
// SAML IdPs carry an "id" so the SPA can request that specific IdP
// (provider=saml:<id>); LDAP is rendered as a username/password form.
func (h *AuthHandlers) ProvidersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		providers := make([]gin.H, 0, 2)
		if h.oidcProvider.Load() != nil {
			providers = append(providers, gin.H{"type": "oidc", "name": "OpenID Connect"})
		}
		for name := range h.samlProviders {
			providers = append(providers, gin.H{"type": "saml", "name": name, "id": name})
		}
		if h.ldapProvider != nil {
			providers = append(providers, gin.H{"type": "ldap", "name": "LDAP"})
		}
		c.JSON(http.StatusOK, gin.H{"providers": providers, "dev_mode": auth.IsDevMode()})
	}
}

// LoginHandler begins the OIDC authorization-code flow. With ?provider=saml (or
// saml:<idp-name>) it begins the SP-initiated SAML flow instead.
func (h *AuthHandlers) LoginHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if provider := c.Query("provider"); provider == "saml" || strings.HasPrefix(provider, "saml:") {
			h.samlLogin(c, provider)
			return
		}
		op := h.oidcProvider.Load()
		if op == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "OIDC provider not configured"})
			return
		}
		state, err := generateState()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate state"})
			return
		}
		// BeginAuth (rather than the legacy GetAuthURL) generates a per-login
		// nonce and PKCE verifier. Both must be persisted alongside the state
		// token so the callback can bind the ID token and code exchange to this
		// specific login attempt.
		challenge, err := op.BeginAuth(state)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to initiate OIDC login"})
			return
		}
		ss := &auth.SessionState{
			State:        state,
			CreatedAt:    time.Now(),
			ProviderType: "oidc",
			Nonce:        challenge.Nonce,
			CodeVerifier: challenge.CodeVerifier,
		}
		if err := h.stateStore.Save(c.Request.Context(), state, ss, 10*time.Minute); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save session state"})
			return
		}
		c.Redirect(http.StatusFound, challenge.URL)
	}
}

// CallbackHandler completes the OIDC flow: exchanges the code, provisions the
// user + membership, issues a JWT, sets the session cookie, and redirects to the
// frontend callback page.
// coverage:skip:requires-oidc-issuer
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

		op := h.oidcProvider.Load()
		if op == nil {
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

		// The PKCE verifier persisted at BeginAuth time binds this exchange to the
		// authorization request this specific login made, so a stolen
		// authorization code cannot be redeemed by anyone who did not also
		// observe the verifier.
		token, err := op.ExchangeCode(ctx, c.Query("code"), idoidc.WithPKCEVerifier(ss.CodeVerifier))
		if err != nil {
			fail("token_exchange_failed", "Failed to exchange the authorization code.")
			return
		}
		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok {
			fail("no_id_token", "The identity provider did not return an ID token.")
			return
		}
		// The nonce persisted at BeginAuth time binds verification to this
		// specific login, so a replayed or injected ID token issued for a
		// different login attempt is rejected.
		idToken, err := op.VerifyIDToken(ctx, rawIDToken, idoidc.WithExpectedNonce(ss.Nonce))
		if err != nil {
			fail("id_token_invalid", "The ID token could not be verified.")
			return
		}
		// The library now also returns an email-verified signal directly; this app
		// still derives that separately via emailVerifiedClaim below (existing
		// enforceEmailVerified path), so it's discarded here rather than plumbed
		// through — reconciling onto the library's value is left to the broader
		// v0.17.0 adoption work.
		sub, email, name, _, err := op.ExtractUserInfo(idToken)
		if err != nil {
			fail("user_info_failed", "Failed to read user information from the ID token.")
			return
		}

		verified := emailVerifiedClaim(idToken)
		if err := enforceEmailVerified(verified, h.cfg.Auth.OIDC.RequireVerifiedEmail); err != nil {
			fail("email_not_verified", err.Error())
			return
		}
		if err := h.guardEmailRebind(ctx, sub, email); err != nil {
			fail("email_bound", err.Error())
			return
		}

		// emailVerified gates the two paths inside GetOrCreateUserByOIDC that
		// establish a NEW email->identity binding (linking a pre-provisioned
		// account or creating a brand-new one); a returning user matched by
		// oidc_sub is unaffected regardless of this value.
		emailVerified := verified != nil && *verified
		user, err := h.userRepo.GetOrCreateUserByOIDC(ctx, sub, email, name, emailVerified)
		if err != nil {
			fail("user_creation_failed", "Failed to look up or create your account.")
			return
		}

		// Map the user's IdP groups (from the verified ID token) to org/role
		// memberships. Groups are never user-supplied here — they come from the
		// signature-verified ID token above. The claim name honours the
		// admin-saved overlay (see effectiveOIDCGroupConfig).
		claimName, _, _ := h.effectiveOIDCGroupConfig(ctx)
		groups := op.ExtractGroups(idToken, claimName)
		if mapErr := h.applyGroupMappings(ctx, user.ID, groups); mapErr != nil {
			slog.Warn("failed to apply OIDC group mappings", "user_id", user.ID, "error", mapErr)
		}

		scopes, err := h.orgRepo.GetUserCombinedScopes(ctx, user.ID)
		if err != nil {
			scopes = []string{}
		}

		jwtToken, err := auth.GenerateJWT(user.ID, user.Email, scopes, sessionTTL)
		if err != nil {
			fail("jwt_failed", "Failed to generate an authentication token.")
			return
		}

		// Attribute the entry to the just-authenticated user: the callback is an
		// unauthenticated route, so the auth middleware has not set user_id.
		c.Set("user_id", user.ID)
		h.audit.write(c, "auth.login", "user", user.ID,
			map[string]interface{}{"provider": "oidc", "email": user.Email})
		h.setSessionCookies(c, jwtToken)
		c.Redirect(http.StatusFound, frontendBase+"/auth/callback")
	}
}

// MeHandler returns the authenticated user, memberships, and combined scopes.
// @Summary      Current user
// @Description  Returns the authenticated user, organization memberships, and combined allowed scopes.
// @Tags         Auth
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /auth/me [get]
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
				"organization_id":      m.OrganizationID,
				"organization_name":    m.OrganizationName,
				"role_template_name":   m.RoleTemplateName,
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
		// The route is optionalAuth: only an authenticated logout is auditable —
		// an anonymous hit must not fabricate an unattributed entry.
		if uid := userIDOf(c); uid != "" {
			h.audit.write(c, "auth.logout", "user", uid, nil)
		}
		h.clearSessionCookies(c)

		postLogout := deriveFrontendURL(h.cfg) + "/"
		if op := h.oidcProvider.Load(); op != nil {
			if endSession := op.GetEndSessionEndpoint(); endSession != "" {
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

// effectiveOIDCGroupConfig returns the OIDC group-mapping settings in force:
// the admin-saved DB overlay when one exists (authoritative, including an empty
// mapping list), else the file config. The claim name falls back file → "groups"
// because an empty claim name cannot address a token claim. Read failures fall
// back to file config so a DB blip can't change login semantics arbitrarily.
func (h *AuthHandlers) effectiveOIDCGroupConfig(ctx context.Context) (claimName string, mappings []config.OIDCGroupMapping, defaultRole string) {
	claimName = h.cfg.Auth.OIDC.GroupClaimName
	mappings = h.cfg.Auth.OIDC.GroupMappings
	defaultRole = h.cfg.Auth.OIDC.DefaultRole

	if h.ssoSettings != nil {
		if s, err := h.ssoSettings.Get(ctx); err != nil {
			slog.Warn("failed to load SSO settings overlay; using file config", "error", err)
		} else if s != nil {
			mappings = make([]config.OIDCGroupMapping, 0, len(s.OIDCGroupMappings))
			for _, m := range s.OIDCGroupMappings {
				mappings = append(mappings, config.OIDCGroupMapping{Group: m.Group, Organization: m.Organization, Role: m.Role})
			}
			defaultRole = s.OIDCDefaultRole
			if s.OIDCGroupClaimName != "" {
				claimName = s.OIDCGroupClaimName
			}
		}
	}
	if claimName == "" {
		claimName = "groups"
	}
	return claimName, mappings, defaultRole
}

// resolveGroupMappings computes, from a user's verified IdP groups and the
// admin-configured mappings, the desired role per organization (orgName -> role,
// last matching mapping wins) and the set of "IdP-managed" organizations (every
// organization referenced by any mapping). Managed organizations are treated as
// IdP-authoritative: a user's membership there must reflect their current groups.
// Pure and side-effect-free so the security-critical decision is unit-tested.
func resolveGroupMappings(groups []string, mappings []config.OIDCGroupMapping) (desired map[string]string, managed map[string]struct{}) {
	desired = make(map[string]string)
	managed = make(map[string]struct{})
	groupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		groupSet[g] = struct{}{}
	}
	for _, m := range mappings {
		managed[m.Organization] = struct{}{}
		if _, ok := groupSet[m.Group]; ok {
			desired[m.Organization] = m.Role
		}
	}
	return desired, managed
}

// applyGroupMappings reconciles the user's organization memberships against their
// verified IdP groups.
//
// SECURITY: groups originate only from the cryptographically-verified ID token
// (see CallbackHandler) and mappings are admin-configured (never user-supplied),
// so a forged group claim cannot drive this.
//
// Hardening over the registry's implementation:
//   - DEPROVISIONING: every organization referenced in group_mappings is
//     IdP-authoritative. On each login we upsert the user's role where a current
//     group maps, and REVOKE their membership in a managed org when no current
//     group maps to it — so losing an IdP group removes the access. Organizations
//     not referenced by any mapping are never touched (manual grants persist).
//   - The default_role fallback is FIRST-LOGIN-ONLY (add only if not already a
//     member) and skips the default org when it is itself IdP-managed, so login
//     can never silently overwrite/re-escalate an existing role (preserves the
//     earlier H4 fix).
func (h *AuthHandlers) applyGroupMappings(ctx context.Context, userID string, groups []string) error {
	_, mappings, defaultRole := h.effectiveOIDCGroupConfig(ctx)
	if len(mappings) == 0 && defaultRole == "" {
		return nil
	}
	desired, managed := resolveGroupMappings(groups, mappings)
	return h.reconcileManagedMemberships(ctx, userID, desired, managed, defaultRole)
}

// reconcileManagedMemberships applies a desired (orgName->role) set against the
// set of IdP-managed organizations: upsert where desired, REVOKE membership in a
// managed org with no desired entry (deprovisioning), and add a first-login-only
// default-role membership in a non-managed default org when nothing was desired.
// Shared by OIDC and LDAP group mapping so both get the same deprovisioning
// semantics. Organizations outside the managed set are never touched.
func (h *AuthHandlers) reconcileManagedMemberships(ctx context.Context, userID string, desired map[string]string, managed map[string]struct{}, defaultRole string) error {
	// Reconcile each IdP-managed organization.
	for orgName := range managed {
		org, err := h.orgRepo.GetByName(ctx, orgName)
		if err != nil || org == nil {
			slog.Warn("group mapping: organization not found", "org", orgName)
			continue
		}
		isMember, _, err := h.orgRepo.CheckMembership(ctx, org.ID, userID)
		if err != nil {
			return fmt.Errorf("check membership org=%s user=%s: %w", org.ID, userID, err)
		}
		if role, want := desired[orgName]; want {
			if isMember {
				if err := h.orgRepo.UpdateMemberRole(ctx, org.ID, userID, role); err != nil {
					return fmt.Errorf("update member role org=%s user=%s: %w", org.ID, userID, err)
				}
			} else if err := h.orgRepo.AddMemberWithParams(ctx, org.ID, userID, role); err != nil {
				return fmt.Errorf("add member org=%s user=%s: %w", org.ID, userID, err)
			}
			slog.Info("group mapping applied", "user_id", userID, "org", orgName, "role", role)
		} else if isMember {
			// Deprovision: member of an IdP-managed org with no matching current group.
			if err := h.orgRepo.RemoveMember(ctx, org.ID, userID); err != nil {
				return fmt.Errorf("revoke membership org=%s user=%s: %w", org.ID, userID, err)
			}
			slog.Info("group mapping: revoked membership (no matching group)", "user_id", userID, "org", orgName)
		}
	}

	// Default-role fallback: first-login-only, and only for a non-managed default org.
	if len(desired) == 0 && defaultRole != "" {
		org, err := h.orgRepo.GetDefaultOrganization(ctx)
		if err != nil || org == nil {
			return fmt.Errorf("default organization not found for default_role fallback")
		}
		if _, isManaged := managed[org.Name]; isManaged {
			return nil // reconciliation above already governs the default org
		}
		isMember, _, err := h.orgRepo.CheckMembership(ctx, org.ID, userID)
		if err != nil {
			return fmt.Errorf("check membership default org user=%s: %w", userID, err)
		}
		if !isMember {
			if err := h.orgRepo.AddMemberWithParams(ctx, org.ID, userID, defaultRole); err != nil {
				return fmt.Errorf("add default member user=%s: %w", userID, err)
			}
		}
	}
	return nil
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
