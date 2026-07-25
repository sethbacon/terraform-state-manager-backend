package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/ldap"
)

// LDAPLoginHandler authenticates a username/password against LDAP, provisions the
// user, reconciles org/role memberships from the user's LDAP groups, and issues
// an HttpOnly session cookie (cookie-only, like the OIDC flow).
//
// NOTE: this password endpoint is protected by an app-layer per-IP rate limit
// (middleware.LoginRateLimit, wired on the route in router.go) to blunt online
// brute-force before the directory bind; a proxy/gateway limit and the LDAP
// directory's own lockout policy apply as additional defense-in-depth.
// @Summary      LDAP login
// @Description  Search-bind authentication against the configured LDAP directory. Sets the session cookie on success.
// @Tags         Auth
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Router       /auth/ldap/login [post]
func (h *AuthHandlers) LDAPLoginHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.ldapProvider == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "LDAP authentication is not enabled"})
			return
		}
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "username and password are required"})
			return
		}

		info, err := h.ldapProvider.Authenticate(req.Username, req.Password)
		if err != nil {
			// The attempted username is auditable; the response stays uniform.
			h.audit.write(c, "auth.login_failed", "user", "",
				map[string]interface{}{"provider": "ldap", "username": req.Username})
			// Uniform message — never reveal whether the user exists vs. bad password.
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}

		ctx := c.Request.Context()
		// Stable identity for an LDAP user is its DN.
		sub := "ldap:" + strings.ToLower(info.DN)
		if err := h.guardEmailRebind(ctx, sub, info.Email); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		// emailVerified=true: this email was just read directly off the LDAP
		// directory entry the user bind-authenticated against above, not a
		// self-asserted OIDC claim — the directory itself is the trust source
		// for this attribute, administratively controlled independent of the
		// end user. That is the same trust level GetOrCreateUserByOIDC's
		// emailVerified gate is designed to require before establishing a new
		// email->identity binding.
		user, err := h.userRepo.GetOrCreateUserByOIDC(ctx, sub, info.Email, info.Name, true)
		if err != nil {
			serverError(c, err, "failed to provision account")
			return
		}

		// Reconcile memberships from LDAP groups using the shared (deprovisioning)
		// reconciler — same semantics as OIDC group mapping.
		desired, managed := ldap.ResolveLDAPGroupMappings(info.Groups, h.cfg.Auth.LDAP.GroupMappings)
		if mapErr := h.reconcileManagedMemberships(ctx, user.ID, desired, managed, h.cfg.Auth.LDAP.DefaultRole); mapErr != nil {
			slog.Warn("failed to apply LDAP group mappings", "user_id", user.ID, "error", mapErr)
		}

		scopes, err := h.orgRepo.GetUserCombinedScopes(ctx, user.ID)
		if err != nil {
			scopes = []string{}
		}
		token, err := auth.GenerateJWT(user.ID, user.Email, scopes, sessionTTL)
		if err != nil {
			serverError(c, err, "failed to generate token")
			return
		}
		// Attribute the entry to the just-authenticated user: this is an
		// unauthenticated route, so the auth middleware has not set user_id.
		c.Set("user_id", user.ID)
		h.audit.write(c, "auth.login", "user", user.ID,
			map[string]interface{}{"provider": "ldap", "email": user.Email})
		h.setSessionCookies(c, token)
		c.JSON(http.StatusOK, gin.H{"expires_in": int(sessionTTL.Seconds())})
	}
}
