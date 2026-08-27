// auth.go implements the HTTP handlers for OIDC login, the OAuth callback, the
// current-user endpoint, token refresh, and logout. It mirrors the registry's
// auth flow, scoped to the OIDC (Keycloak) provider used for local development.
package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	identitymodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	idoidc "github.com/sethbacon/terraform-suite-identity/identity/auth/oidc"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/ldap"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/saml"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/credlifecycle"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/middleware"
)

const sessionTTL = 24 * time.Hour

// AuthHandlers serves the authentication endpoints.
type AuthHandlers struct {
	cfg      *config.Config
	userRepo *idstore.UserRepository
	// orgRepo carries TSM's per-app role mirror: the IdP group reconciliation
	// below is the highest-volume role writer in this application, and every one
	// of its grants, reassignments and revocations is dual-written through it.
	orgRepo       *approles.Members
	roleRepo      *idstore.RoleTemplateRepository
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
	// creds invalidates credentials the IdP group-mapping reconciliation has
	// just made too broad. May be nil (no sweep).
	creds *credlifecycle.Sweeper
}

// AuthOption configures optional AuthHandlers construction behaviour.
type AuthOption func(*AuthHandlers)

// WithAuthCredentialSweeper wires the credential sweep the IdP group-mapping
// reconciliation performs when a login reduces a user's memberships or roles.
func WithAuthCredentialSweeper(s *credlifecycle.Sweeper) AuthOption {
	return func(h *AuthHandlers) { h.creds = s }
}

// NewAuthHandlers constructs the auth handlers. identityDB must resolve to the
// identity schema (search_path=identity,public). The OIDC provider is initialised
// only when OIDC is enabled in config.
//
// appDB is the APPLICATION connection, which is where TSM's own role tables
// live; it is what attaches the per-app role mirror to every membership write
// the IdP group reconciliation performs. See NewAdminHandlers for why it is a
// parameter and not an option.
func NewAuthHandlers(cfg *config.Config, identityDB, appDB *sql.DB, opts ...AuthOption) (*AuthHandlers, error) {
	// The login state store must be shared across replicas: the login redirect
	// (Save) and the callback (Load) are separate HTTP requests, and behind a
	// load balancer they land on different pods — a per-process map fails
	// (N-1)/N of SSO logins there. The DB store is durable and single-use
	// across the fleet; the memory store remains for nil-DB test rigs.
	var stateStore auth.StateStore = auth.NewMemoryStateStore()
	if identityDB != nil {
		stateStore = repositories.NewLoginStateRepository(identityDB)
	}
	h := &AuthHandlers{
		cfg:         cfg,
		userRepo:    idstore.NewUserRepository(identityDB),
		orgRepo:     approles.NewMembers(identityDB, appDB, approles.RoleSource(cfg.Authz.RoleSource)),
		roleRepo:    idstore.NewRoleTemplateRepository(identityDB),
		tokenRepo:   idstore.NewTokenRepository(identityDB),
		apiKeyRepo:  idstore.NewAPIKeyRepository(identityDB),
		stateStore:  stateStore,
		ssoSettings: repositories.NewSSOSettingsRepository(identityDB),
		audit:       newAuditor(identityDB),
	}
	for _, opt := range opts {
		opt(h)
	}
	if cfg.Auth.OIDC.Enabled {
		p, err := auth.NewOIDCProvider(&cfg.Auth.OIDC)
		if err != nil {
			return nil, err
		}
		h.oidcProvider.Store(p)
	}
	if cfg.Auth.LDAP.Enabled {
		ldapCfg := cfg.Auth.LDAP
		if ldapCfg.InsecureSkipVerify && !auth.IsDevMode() {
			// insecure_skip_verify disables LDAPS/StartTLS certificate verification,
			// exposing the service-account bind credential and all directory traffic
			// (including user passwords) to a man-in-the-middle. Honor it only in dev
			// mode; force verification on in production (#251), mirroring the OIDC
			// AllowInsecureIssuer=IsDevMode() gate.
			slog.Warn("ldap.insecure_skip_verify is ignored outside dev mode; enforcing LDAP certificate verification")
			ldapCfg.InsecureSkipVerify = false
		}
		p, err := ldap.NewProvider(ldapCfg)
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

// loginScope is the tenancy every identity accessor on an AUTHENTICATION path
// carries.
//
// It is OrgScopeAllOrganizations(), and it is the case UPGRADING.md names
// explicitly: "an authority-derivation path (login, API-key authentication, a
// middleware that is itself the tenant check)". Two shapes reach it, and
// neither has a tenant to scope by:
//
//   - /auth/me and /auth/refresh read the CALLER'S OWN record to report or
//     re-derive their scope union. The scope that would narrow the read is the
//     one the read exists to compute, so any narrower value is circular — and a
//     value derived from the presented token would let the token decide which
//     rows authenticate it.
//   - The OIDC/LDAP group-mapping reconciliation resolves an organization NAMED
//     BY AN ADMIN-CONFIGURED MAPPING (never by the user, never by the ID token).
//     Once resolved, every membership read and write below is scoped to that one
//     organization's id, so the platform-wide reach stops at the name lookup.
//
// Recorded in admin_audit_scope_test.go's reviewed list of platform-wide sites.
// Nothing here reads audit_logs.
func loginScope() idstore.OrgScope { return idstore.OrgScopeAllOrganizations() }

// UserRepo and TokenRepo expose the identity repositories for the auth middleware.
func (h *AuthHandlers) UserRepo() *idstore.UserRepository     { return h.userRepo }
func (h *AuthHandlers) TokenRepo() *idstore.TokenRepository   { return h.tokenRepo }
func (h *AuthHandlers) APIKeyRepo() *idstore.APIKeyRepository { return h.apiKeyRepo }
func (h *AuthHandlers) OrgRepo() *approles.Members            { return h.orgRepo }

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
			serverError(c, err, "Failed to generate state")
			return
		}
		// BeginAuth generates a per-login nonce and PKCE verifier and returns
		// them as one CallbackSession. Both must be persisted alongside the state
		// token so the callback can bind the ID token and code exchange to this
		// specific login attempt; ExchangeAndVerify refuses the exchange if
		// either is missing, so a half-written state entry fails the login rather
		// than completing an unbound one.
		challenge, err := op.BeginAuth(state)
		if err != nil {
			serverError(c, err, "Failed to initiate OIDC login")
			return
		}
		ss := &auth.SessionState{
			State:        state,
			CreatedAt:    time.Now(),
			ProviderType: "oidc",
			Nonce:        challenge.Session.Nonce,
			CodeVerifier: challenge.Session.CodeVerifier,
		}
		if err := h.stateStore.Save(c.Request.Context(), state, ss, 10*time.Minute); err != nil {
			serverError(c, err, "Failed to save session state")
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

		// Both per-login bindings persisted at BeginAuth time go back as one
		// value: the PKCE verifier binds this exchange to the authorization
		// request this specific login made (so a stolen authorization code
		// cannot be redeemed by anyone who did not also observe the verifier),
		// and the nonce binds verification to the same login (so a replayed or
		// injected ID token issued for a different attempt is rejected).
		//
		// ExchangeAndVerify applies both itself and fails closed on an empty
		// one before any network call, which also covers the deploy-window case:
		// a state entry written by the previous build carries both fields, but
		// one written by anything that lost a field now fails the callback
		// instead of completing an unbound exchange. It extracts id_token from
		// the token response, so there is no separate "no ID token" branch.
		_, idToken, err := op.ExchangeAndVerify(ctx, c.Query("code"), idoidc.CallbackSession{
			Nonce:        ss.Nonce,
			CodeVerifier: ss.CodeVerifier,
		})
		if err != nil {
			fail("token_exchange_failed", "Failed to exchange the authorization code.")
			return
		}
		// oidcEmailVerified is the IdP's email_verified signal for THIS login,
		// as returned by ExtractUserInfo (terraform-suite-identity's fix for
		// audit #52). It gates GetOrCreateUserFromOIDC's new email->identity
		// binding paths below and is distinct from the enforceEmailVerified
		// check just after: that one implements this app's own configurable
		// RequireVerifiedEmail login-rejection policy, re-derived independently
		// from the raw ID token claim via emailVerifiedClaim.
		sub, email, name, oidcEmailVerified, err := op.ExtractUserInfo(idToken)
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

		user, err := h.userRepo.GetOrCreateUserFromOIDC(ctx, sub, email, name, oidcEmailVerified)
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

// effectiveScopesOf returns the scope set the auth middleware published for THIS
// request, or nil when the key was never set.
//
// nil and empty are deliberately not distinguished by the callers: both mean "no
// authority was established here", and reportedScopes fails closed on either.
func effectiveScopesOf(c *gin.Context) []string {
	v, ok := c.Get("scopes")
	if !ok {
		return nil
	}
	scopes, _ := v.([]string)
	return scopes
}

// reportedScopes builds the allowed_scopes /auth/me publishes: the caller's live
// role-template union, with its `admin` bit reconciled against the authority
// actually in force for this request.
//
// GUARD me-allowed-scopes-carrier (#391).
//
// approles.Members.GetUserCombinedScopes reads THIS APPLICATION'S ROLE TABLES
// and nothing else, which is only one of the two carriers of platform-admin
// authority. The other is the platform_admins table, which
// middleware.AuthMiddleware resolves per request (elevate ->
// platformadmin.Service.SessionScopes) and publishes as the request's `scopes`.
// A principal whose `admin` comes only from the carrier is therefore authorized
// at every server-side HasScope(ScopeAdmin) site while /auth/me reports them an
// ordinary user — and the frontend gates all admin navigation on this field, so
// they are shown no route to the authority they actually hold.
//
// TSM IS MORE EXPOSED HERE THAN REGISTRY WAS. Registry's migration 000051
// backfilled its carrier from admin-bearing role templates, so its two answers
// agreed by construction until the union stopped carrying `admin` at all. TSM's
// migration 000030 says "NO BACKFILL" in as many words, so the carrier and the
// role union are independent from the very first grant.
//
// IT SUBTRACTS AS WELL AS ADDS — BUT NOT FOR REGISTRY'S REASON, and the
// difference is why this is not a transplant. Registry has retired role-template
// `admin`: it strips a scope that confers nothing at all. TSM's model is
// ADDITIVE (platformadmin.Service.SessionScopes: effective admin is `carrier OR
// the presented session's own scope union`), so a role-template `admin` is real
// authority and must never be stripped merely for being a template's. What is
// stripped is a role-template `admin` THIS REQUEST DOES NOT ACTUALLY CARRY,
// which under the additive model is a strictly narrower set — and non-empty:
//
//   - API-key authentication. middleware.authenticateAPIKey strips `admin`
//     unconditionally (idplatformadmin.KeyScopes), on purpose: an unattended CI
//     credential must not inherit its owner's platform-admin. The live role union
//     knows nothing of that, so without the subtraction /auth/me tells a pipeline
//     token it is an administrator while every admin call it makes answers 403.
//   - An admin-bearing role template granted since the session token was minted.
//     The middleware elevates from the token's claims, so the server denies until
//     the next refresh; reporting `admin` renders admin navigation that 403s.
//
// So the rule is a single one: `admin` appears in allowed_scopes exactly when it
// is in force for this request. Read from the request's EFFECTIVE scopes rather
// than by querying the carrier a second time, so this stays downstream of the
// middleware's single insertion point instead of becoming a second,
// independently-driftable answer to the same question — and so it inherits that
// point's deliberate exclusion of API keys for free.
//
// FAIL CLOSED on an absent `scopes` key. Every authenticated path publishes it
// (middleware.setAuthContext, the API-key branch, and mtls.AuthMiddleware), and
// /auth/me is mounted behind requireAuth, so an absent one is a mis-wired route
// rather than a principal and must not be answered with the union's wildcard.
//
// ONLY the `admin` bit is reconciled, which is the whole of the divergence: every
// other scope has exactly one carrier — the role tables this union is read from —
// so there is no second answer for them to disagree with.
func reportedScopes(union, effective []string) []string {
	out := make([]string, 0, len(union)+1)
	for _, s := range union {
		if s != string(auth.ScopeAdmin) {
			out = append(out, s)
		}
	}
	if auth.HasScope(effective, auth.ScopeAdmin) {
		out = append(out, string(auth.ScopeAdmin))
	}
	return out
}

// MeHandler returns the authenticated user, memberships, and combined scopes.
// @Summary      Current user
// @Description  Returns the authenticated user, organization memberships, and the allowed scopes in force for this session. `admin` is reported when this request actually carries it, whether it was conferred by a role template or by the platform-admin carrier, and is absent otherwise — notably on API-key authentication, which never inherits its owner's platform-admin.
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

		// Sentinel first: a token naming a deleted user must keep answering 404,
		// not 500.
		//
		// GetUserByID, NOT GetUserWithOrgRoles, since
		// sethbacon/terraform-suite-identity#206 Phase 3b. That accessor resolves
		// each membership's role by joining the SHARED identity schema's
		// role_templates, which is no longer where this application's roles live —
		// so its Memberships would have put identity's role name and scopes in the
		// same response as allowed_scopes, which comes from THIS application's
		// tables. On a coupled deployment /auth/me would then show a principal one
		// role while granting them another, on the one endpoint whose whole job is
		// to tell a user what they are. It propagates the same ErrNotFound (that
		// is where GetUserWithOrgRoles got it), and dropping it also drops the
		// membership query whose result is now discarded.
		user, err := h.userRepo.GetUserByID(c.Request.Context(), uid, loginScope())
		if errors.Is(err, idstore.ErrNotFound) || (err == nil && user == nil) {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}
		if err != nil {
			serverError(c, err, "Failed to get user information")
			return
		}

		// Unscoped for the same reason GetUserCombinedScopes below is: the caller
		// is asking about themselves.
		userMemberships, mErr := h.orgRepo.GetUserMemberships(c.Request.Context(), uid)
		if mErr != nil {
			serverError(c, mErr, "Failed to get user information")
			return
		}

		scopes, err := h.orgRepo.GetUserCombinedScopes(c.Request.Context(), uid)
		if err != nil {
			scopes = []string{}
		}
		scopes = reportedScopes(scopes, effectiveScopesOf(c))
		memberships := make([]gin.H, 0, len(userMemberships))
		for _, m := range userMemberships {
			memberships = append(memberships, gin.H{
				"organization_id":      m.OrganizationID,
				"organization_name":    m.OrganizationName,
				"role_template_name":   m.RoleTemplateName,
				"role_template_scopes": m.RoleTemplateScopes,
			})
		}

		resp := gin.H{
			"user": gin.H{
				"id":    user.ID,
				"email": user.Email,
				"name":  user.Name,
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

// sessionAuthMethods are the authentication methods that constitute an
// INTERACTIVE SESSION: a JWT this server minted through a login flow, presented
// either in the auth cookie (jwt_cookie) or in an Authorization header (jwt).
// Everything else behind requireAuth is a machine credential with its own,
// independent lifecycle — an API key (rotated at /apikeys/:id/rotate, expiring
// on its own expires_at, swept by credlifecycle) or an mTLS client certificate
// (whose scopes come from a cert mapping and which has no user at all).
//
// ALLOWLIST, NOT DENYLIST (#339). A future authentication method has to opt in
// here deliberately; one added to middleware.AuthMiddleware without a thought
// for session minting is refused rather than admitted by default.
var sessionAuthMethods = map[string]bool{"jwt": true, "jwt_cookie": true}

// isInteractiveSession reports whether this request was authenticated by a
// session JWT rather than by a machine credential.
//
// FAILS CLOSED on an absent auth_method. Every authenticated path publishes one
// — middleware.setAuthContext for both JWT forms, the API-key branch of
// middleware.authenticateAPIKey, and mtls.AuthMiddleware — so an absent one is
// a mis-wired route rather than a principal, and must not be read as "session".
func isInteractiveSession(c *gin.Context) bool {
	v, ok := c.Get("auth_method")
	if !ok {
		return false
	}
	m, _ := v.(string)
	return sessionAuthMethods[m]
}

// RefreshHandler issues a new JWT with fresh scopes and revokes the old one.
//
// INTERACTIVE SESSIONS ONLY (#339). requireAuth is middleware.AuthMiddleware,
// which also authenticates API keys, so before this check a key could present
// itself here and be handed a session cookie. That was an authority ceiling
// derived from the OWNING USER instead of from the PRESENTING CREDENTIAL, and
// it defeated both narrowings the API-key path applies:
//
//   - middleware.grantedSubset caps a key's stored scopes by its owner's live
//     set, so a key deliberately minted with only state:read stays state:read.
//     Refresh read the owner's whole cross-organization union instead.
//   - idplatformadmin.KeyScopes strips `admin` from an API-key request
//     UNCONDITIONALLY, precisely because an unattended CI credential must not
//     inherit its owner's platform-admin. A refresh routed AROUND that strip
//     handed an admin owner's narrowed CI key a session cookie carrying `admin`.
//
// REFUSED OUTRIGHT rather than re-derived from the key's own scopes, because a
// scope-correct answer would still be the wrong shape. Refresh does not merely
// restate authority, it CHANGES THE CREDENTIAL KIND: the session it mints is
// not bound to the key's expires_at, is not revoked when the key is deleted, is
// invisible to credlifecycle.Sweeper (which sweeps the key family and leaves
// sessions to their TTL), and is a cookie credential rather than a bearer one.
// Even at identical scopes that is an escalation in DURABILITY and in
// revocability, so bounding the scope set would close only half the hole while
// adding a third place that has to remember the `admin` strip. There is also
// nothing to refresh: keys do not expire the way sessions do, and a key that
// needs new material rotates at POST /apikeys/:id/rotate.
//
// Logged, not audited. A refused refresh writes no database row, so a rejected
// path cannot be used to pump the audit table; the key's presentation is
// already recorded by the middleware's UpdateLastUsed.
func (h *AuthHandlers) RefreshHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := c.Get("user_id")
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}
		uid, _ := userID.(string)

		// Before any lookup: authority here must come from the credential that
		// presented the request, and only a session has a session to refresh.
		if !isInteractiveSession(c) {
			method, _ := c.Get("auth_method")
			slog.Warn("refresh refused: not an interactive session",
				"user_id", uid, "auth_method", method)
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "Session refresh requires an interactive session",
				"details": "API keys and mTLS client certificates are not sessions and cannot be exchanged for one; rotate an API key at POST /api/v1/apikeys/{id}/rotate instead",
			})
			return
		}

		user, err := h.userRepo.GetUserByID(c.Request.Context(), uid, loginScope())
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
			serverError(c, err, "Failed to generate new token")
			return
		}
		h.setSessionCookies(c, newToken)
		// Cookie-only sessions: the JWT is delivered as an HttpOnly cookie and is
		// never returned in the body (so it cannot be read or persisted by JS).
		c.JSON(http.StatusOK, gin.H{"expires_in": int(sessionTTL.Seconds())})
	}
}

// LogoutPostHandler ends the session under CSRF protection.
// @Summary      Log out (CSRF-safe)
// @Description  Revokes the session JWT, clears the auth and CSRF cookies, and returns where the client should navigate next — the OIDC end-session endpoint when one is configured, otherwise the frontend root. Unlike GET /auth/logout this is subject to the double-submit CSRF check, so it cannot be triggered by a cross-site navigation. Returns 200 with a redirect_url rather than a 302 because an XHR cannot follow a cross-origin redirect to the IdP.
// @Tags         Authentication
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Router       /auth/logout [post]
//
// LogoutPostHandler is the CSRF-safe counterpart to LogoutHandler (#274). The
// GET route is reachable by a cross-site top-level navigation — SameSite=Lax
// still sends the auth cookie — so an attacker-controlled link can force a
// logout. CSRFProtect deliberately skips safe methods, so the only way to bring
// logout under the double-submit check is to make it a POST.
//
// It answers 200 with the post-logout destination in the body rather than a 302:
// under OIDC that destination is the provider's end-session endpoint, and an XHR
// cannot usefully follow a cross-origin redirect, so the SPA must navigate
// itself. The GET route stays for now — the shared frontend drives logout for
// this app and the sibling registry, so both backends have to accept POST before
// either can drop GET.
func (h *AuthHandlers) LogoutPostHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"redirect_url": h.endSession(c)})
	}
}

// endSession performs the session teardown both logout routes share — revoke,
// audit, clear cookies — and returns where the caller should end up. Kept as one
// function so the two verbs cannot drift apart: the POST route exists to change
// the CSRF posture, not the logout semantics.
func (h *AuthHandlers) endSession(c *gin.Context) string {
	h.revokeCurrent(c)
	// The routes are optionalAuth: only an authenticated logout is auditable —
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
				return u.String()
			}
		}
	}
	return postLogout
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
// admin-configured mappings, the desired role per organization, the set of
// "IdP-managed" organizations (every organization any mapping references), and
// EVERY role a matching mapping resolved to per organization.
//
// PRECEDENCE NOW COMES FROM THE SHARED MODULE (#488, identity#269). This app
// took the LAST matching mapping and the registry took the FIRST, off the same
// shared type, so one stored mapping list granted different roles depending on
// which app read it. First-wins is the estate's canonical rule: appending a
// mapping cannot then change the outcome for anyone already matched, which is
// what an authorization list edited incrementally through a UI needs.
//
// WHY `allMatching` EXISTS, AND WHY IT IS NOT JUST THE WINNER. The admin
// preservation this reconciler depends on used to be an emergent property of
// ORDERING: a mapping resolving to an admin role is refused by
// guardProvisionableRole, which deliberately does not fall through to the
// revoke branch, so a matching-but-refused admin mapping was the only supported
// way to hold a manual admin grant in an IdP-managed organization -- and that
// only worked while the admin mapping was the one that WON.
//
// Under any precedence change that mechanism breaks silently: a weaker mapping
// wins instead, PASSES the guard, and demotes a real administrator with no
// error anywhere. Returning every matching role lets the caller ask the
// question that actually matters -- "does ANY mapping for this organization
// resolve to a role I may not auto-provision?" -- which is true regardless of
// which one wins. The preservation stops depending on array order.
//
// Pure and side-effect-free so the security-critical decision is unit-tested.
func resolveGroupMappings(groups []string, mappings []config.OIDCGroupMapping) (desired map[string]string, managed map[string]struct{}, allMatching map[string][]string) {
	shared := make([]identitymodels.OIDCGroupMapping, 0, len(mappings))
	for _, m := range mappings {
		shared = append(shared, identitymodels.OIDCGroupMapping{
			Group: m.Group, Organization: m.Organization, Role: m.Role,
		})
	}
	res := identitymodels.ResolveGroupMappings(groups, shared)

	desired = res.DesiredRole
	managed = res.Managed

	groupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		groupSet[g] = struct{}{}
	}
	allMatching = make(map[string][]string)
	for _, m := range mappings {
		if _, held := groupSet[m.Group]; !held {
			continue
		}
		allMatching[m.Organization] = append(allMatching[m.Organization], m.Role)
	}
	return desired, managed, allMatching
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
//   - A mapped group whose resolved role_template carries auth.ScopeAdmin is
//     refused rather than auto-applied (see guardProvisionableRole, #173):
//     defense-in-depth so an IdP-driven mapping can never silently grant the
//     grant-all wildcard scope.
func (h *AuthHandlers) applyGroupMappings(ctx context.Context, userID string, groups []string) error {
	_, mappings, defaultRole := h.effectiveOIDCGroupConfig(ctx)
	if len(mappings) == 0 && defaultRole == "" {
		return nil
	}
	desired, managed, allMatching := resolveGroupMappings(groups, mappings)
	return h.reconcileManagedMemberships(ctx, userID, desired, managed, allMatching, defaultRole)
}

// reconcileManagedMemberships applies a desired (orgName->role) set against the
// set of IdP-managed organizations: upsert where desired, REVOKE membership in a
// managed org with no desired entry (deprovisioning), and add a first-login-only
// default-role membership in a non-managed default org when nothing was desired.
// Shared by OIDC and LDAP group mapping so both get the same deprovisioning
// semantics. Organizations outside the managed set are never touched.
// allMatching carries EVERY role a matching mapping resolved to per
// organization, not only the winner. The admin-preservation guard below asks
// about all of them, so that preservation does not depend on which mapping the
// precedence rule happened to pick -- see resolveGroupMappings' doc (#488).
func (h *AuthHandlers) reconcileManagedMemberships(ctx context.Context, userID string, desired map[string]string, managed map[string]struct{}, allMatching map[string][]string, defaultRole string) error {
	// Reconcile each IdP-managed organization.
	for orgName := range managed {
		org, err := h.orgRepo.GetByName(ctx, orgName, loginScope())
		if err != nil || org == nil {
			slog.Warn("group mapping: organization not found", "org", orgName)
			continue
		}
		isMember, _, err := h.orgRepo.CheckMembership(ctx, org.ID, userID, idstore.OrgScopeOrganizations(org.ID))
		if err != nil {
			return fmt.Errorf("check membership org=%s user=%s: %w", org.ID, userID, err)
		}
		if role, want := desired[orgName]; want {
			// Refuse the automatic, IdP-driven role assignment when the resolved
			// role_template carries auth.ScopeAdmin — see guardProvisionableRole's
			// doc for the full rationale (#173). Deliberately does NOT fall
			// through to the revoke branch below: rejection must leave an
			// existing (possibly unrelated, legitimately-granted) membership
			// untouched, not tear it down.
			// ASK ABOUT EVERY MATCHING MAPPING, NOT ONLY THE WINNER (#488).
			//
			// Guarding just `role` made the preservation depend on the guarded
			// mapping being the one that won, which was true under last-wins
			// only because operators ordered their arrays for it. Under
			// first-wins the same configuration lets a weaker mapping win, PASS
			// this guard, and demote a real administrator -- no error, no
			// warning, visible only as lost authority at the next login.
			//
			// So the question is asked of every role any matching mapping
			// resolved to for this organization. If ANY of them may not be
			// auto-provisioned, the organization is left alone entirely. That is
			// the same outcome the old code produced for a correctly-ordered
			// array, and now it does not depend on the order at all.
			// FALL BACK TO THE WINNER, NEVER TO NOTHING. A caller that supplies
			// no list -- or an organization with no entry in it -- must still get
			// the original per-winner guard. Degrading to an empty loop would
			// silently disable the admin refusal entirely, which is a far worse
			// failure than the ordering dependence being fixed here.
			candidates := allMatching[orgName]
			if len(candidates) == 0 {
				candidates = []string{role}
			}
			refused := ""
			var refusedErr error
			for _, candidate := range candidates {
				if guardErr := h.guardProvisionableRole(ctx, candidate); guardErr != nil {
					refused, refusedErr = candidate, guardErr
					break
				}
			}
			if refusedErr != nil {
				slog.Warn("group mapping rejected: a matching role is not automatically provisionable by an IdP-driven mapping; a human admin must grant it explicitly",
					"user_id", userID, "org", orgName, "winning_role", role,
					"refused_role", refused, "error", refusedErr)
				continue
			}
			if isMember {
				// ErrNotFound means the membership went away between the
				// CheckMembership above and this write (a concurrent admin
				// removal, or a parallel login reconciling the same user). The
				// reconciliation is a per-organization loop over the whole
				// managed set: aborting it here would leave every LATER
				// organization unreconciled and fail the login outright, for an
				// element that is already in a settled state. Skip and continue.
				// KEYS ONLY, and that asymmetry is the documented one: this runs
				// microseconds before the same request mints the user's session
				// token, and moving the JWT watermark here would revoke the token
				// being issued (see credlifecycle.Sweeper.KeysOnly and
				// sweepIdPReduction). Supplying the flavour per call site is
				// exactly why the reducer is the mutation's argument.
				updErr := h.orgRepo.UpdateMemberRole(ctx, org.ID, userID, role, idstore.OrgScopeOrganizations(org.ID),
					h.sweepIdPReduction("idp group mapping: role reassigned"))
				if errors.Is(updErr, idstore.ErrNotFound) {
					slog.Info("group mapping: membership vanished before the role update; skipping",
						"user_id", userID, "org", orgName)
					continue
				}
				if updErr != nil {
					return fmt.Errorf("update member role org=%s user=%s: %w", org.ID, userID, updErr)
				}
			} else if err := h.orgRepo.AddMemberWithParams(ctx, org.ID, userID, role, idstore.OrgScopeOrganizations(org.ID),
				h.sweepIdPReduction("idp: group mapping applied")); err != nil {
				return fmt.Errorf("add member org=%s user=%s: %w", org.ID, userID, err)
			}
			slog.Info("group mapping applied", "user_id", userID, "org", orgName, "role", role)
		} else if isMember {
			// Deprovision: member of an IdP-managed org with no matching current group.
			//
			// ErrNotFound means the membership is ALREADY gone — the desired end
			// state of this very branch. Treat it as applied and carry on
			// reconciling the remaining organizations rather than aborting the
			// login; a deprovisioning loop that stops at the first
			// already-deprovisioned element leaves the rest provisioned.
			if err := h.orgRepo.RemoveMember(ctx, org.ID, userID, idstore.OrgScopeOrganizations(org.ID),
				h.sweepIdPReduction("idp group mapping: membership revoked")); err != nil &&
				!errors.Is(err, idstore.ErrNotFound) {
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
		isMember, _, err := h.orgRepo.CheckMembership(ctx, org.ID, userID, idstore.OrgScopeOrganizations(org.ID))
		if err != nil {
			return fmt.Errorf("check membership default org user=%s: %w", userID, err)
		}
		if !isMember {
			if err := h.orgRepo.AddMemberWithParams(ctx, org.ID, userID, defaultRole, idstore.OrgScopeOrganizations(org.ID),
				h.sweepIdPReduction("idp: default-role membership applied")); err != nil {
				return fmt.Errorf("add default member user=%s: %w", userID, err)
			}
		}
	}
	return nil
}

// sweepIdPReduction invalidates the API keys an IdP-driven membership or role
// change has just made too broad. Best-effort: the authority change has already
// committed, and a login must not fail because a credential sweep did (#330).
//
// It deliberately sweeps only the API-key family. The JWT half is left to the
// session this very request is about to mint: reconciliation runs microseconds
// before auth.GenerateJWT, the revoke-all watermark is written at full
// precision while a JWT's iat is floored to the second (RFC 7519), and
// TokensRevokedSince resolves that same-second ambiguity toward "revoked" — so
// moving the watermark here would revoke the token being issued and the user
// could never log in. The new token is derived from GetUserCombinedScopes AFTER
// the change committed, so it already carries the reduced authority. The
// residual is the user's OTHER live sessions from earlier logins, which keep
// the pre-reduction scope union until their 24h TTL expires; an operator who
// needs those retired immediately must use the admin membership routes, which
// call AuthorityReduced and do move the watermark. See
// credlifecycle.Sweeper.KeysOnly.
func (h *AuthHandlers) sweepIdPReduction(reason string) approles.AuthorityReducer {
	return func(ctx context.Context, userID string, authorityChanged bool) error {
		out := h.creds.KeysOnly(ctx, userID, reason)
		if out.KeysRevoked > 0 || out.Incomplete {
			slog.Info("idp reconciliation: credential sweep",
				"user_id", userID, "reason", reason,
				"api_keys_revoked", out.KeysRevoked, "api_keys_retained", out.KeysRetained,
				"incomplete", out.Incomplete)
		}
		// Never fails the login: the authority change has already committed, and
		// a login that 500s because a credential sweep did is a worse outcome
		// than the residual the sweep was closing (#330).
		return nil
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
	isMember, _, err := h.orgRepo.CheckMembership(ctx, org.ID, userID, idstore.OrgScopeOrganizations(org.ID))
	if err != nil {
		return
	}
	if isMember {
		// First-login-only: never re-assign or escalate an existing member.
		return
	}
	// KEYS ONLY, like every other authority write on a login path — see
	// sweepIdPReduction: moving the JWT watermark microseconds before
	// GenerateJWT would revoke the very token this request is about to mint.
	//
	// It is not a no-op even though the CheckMembership above establishes the
	// principal is not a member: that boolean is identity's fact, and this
	// application can still hold a stale role record for the pair (CheckDrift's
	// `stale` kind), which the mirror's upsert then moves down to `role`.
	if err := h.orgRepo.AddMemberWithParams(ctx, org.ID, userID, role, idstore.OrgScopeOrganizations(org.ID),
		h.sweepIdPReduction("login: first-login role assignment")); err != nil {
		slog.Warn("failed to add membership", "user_id", userID, "role", role, "error", err)
	}
}
