package api

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/saml"
)

// samlLogin begins the SP-initiated SAML flow for ?provider=saml or
// saml:<idp-name>. It mints a single-use state token, records the AuthnRequest ID
// in session state (so the ACS can validate InResponseTo), and redirects the
// browser to the IdP. Called from LoginHandler.
// coverage:skip:requires-saml-idp
func (h *AuthHandlers) samlLogin(c *gin.Context, provider string) {
	if len(h.samlProviders) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "SAML provider not configured"})
		return
	}

	// "saml" uses the first configured IdP; "saml:<name>" selects by name.
	idpName := ""
	if strings.HasPrefix(provider, "saml:") {
		idpName = strings.TrimPrefix(provider, "saml:")
	}
	if idpName == "" {
		for name := range h.samlProviders {
			idpName = name
			break
		}
	}
	sp, ok := h.samlProviders[idpName]
	if !ok || sp == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("SAML IdP %q not configured", idpName)})
		return
	}

	state, err := generateState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate state"})
		return
	}
	redirectURL, requestID, err := sp.MakeAuthenticationRequest(state)
	if err != nil {
		slog.Error("saml: failed to create AuthnRequest", "idp", idpName, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to initiate SAML login"})
		return
	}

	ss := &auth.SessionState{
		State:        state,
		CreatedAt:    time.Now(),
		ProviderType: "saml:" + idpName,
		RequestID:    requestID,
	}
	if err := h.stateStore.Save(c.Request.Context(), state, ss, 10*time.Minute); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save session state"})
		return
	}
	c.Redirect(http.StatusFound, redirectURL.String())
}

// SAMLMetadataHandler returns the SP metadata XML for the named (or first
// configured) IdP, for publishing to the SAML identity provider.
// @Summary      SAML SP metadata
// @Description  Returns the SAML Service Provider metadata XML for the named (or first configured) IdP. Used by SAML identity providers during federation setup.
// @Tags         Auth
// @Produce      xml
// @Param        idp  query  string  false  "SAML IdP name (defaults to first configured)"
// @Success      200  {string}  string  "SAML SP metadata XML"
// @Failure      404  {object}  map[string]interface{}
// @Router       /auth/saml/metadata [get]
// coverage:skip:requires-saml-idp
func (h *AuthHandlers) SAMLMetadataHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var provider *saml.Provider
		if name := c.Query("idp"); name != "" {
			provider = h.samlProviders[name]
		} else {
			for _, p := range h.samlProviders {
				provider = p
				break
			}
		}
		if provider == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "No SAML provider configured"})
			return
		}
		data, err := xml.MarshalIndent(provider.GetMetadata(), "", "  ")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to marshal SP metadata"})
			return
		}
		c.Data(http.StatusOK, "application/samlmetadata+xml", data)
	}
}

// SAMLACSHandler is the SAML Assertion Consumer Service. It validates the IdP's
// signed response, provisions the user, reconciles org/role memberships from the
// SAML group attribute (deprovisioning), and issues an HttpOnly session cookie —
// cookie-only, like the OIDC and LDAP flows.
//
// SECURITY: assertion trust (signature, XSW round-trip, replay window,
// audience/recipient/destination) is enforced by the provider (see the saml
// package). For SP-initiated logins the AuthnRequest ID recorded at login is
// passed back here so the response's InResponseTo is validated; the IdP-initiated
// fallback is only accepted when auth.saml.allow_idp_initiated is set.
// @Summary      SAML Assertion Consumer Service
// @Description  Receives a signed SAML response from the IdP (SP- or IdP-initiated), validates the assertion, provisions the user, and sets the session cookie before redirecting to the frontend callback.
// @Tags         Auth
// @Accept       x-www-form-urlencoded
// @Produce      html
// @Param        SAMLResponse  formData  string  true   "Base64-encoded SAML response"
// @Param        RelayState    formData  string  false  "Relay state (the SP-initiated session key)"
// @Success      302  {string}  string  "Redirects to the frontend /auth/callback"
// @Failure      400  {object}  map[string]interface{}
// @Router       /auth/saml/acs [post]
// coverage:skip:requires-saml-idp
func (h *AuthHandlers) SAMLACSHandler() gin.HandlerFunc {
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

		if len(h.samlProviders) == 0 {
			fail("provider_not_configured", "SAML is not configured.")
			return
		}

		ctx := c.Request.Context()
		relayState := c.PostForm("RelayState")

		var provider *saml.Provider
		var idpName string
		var possibleRequestIDs []string

		// SP-initiated: RelayState is our single-use state key recording the IdP and
		// the AuthnRequest ID. Binding the request ID lets the provider validate
		// InResponseTo against the request we issued.
		if relayState != "" {
			if ss, _ := h.stateStore.Load(ctx, relayState); ss != nil && strings.HasPrefix(ss.ProviderType, "saml:") {
				idpName = strings.TrimPrefix(ss.ProviderType, "saml:")
				provider = h.samlProviders[idpName]
				if ss.RequestID != "" {
					possibleRequestIDs = []string{ss.RequestID}
				}
			}
		}

		// IdP-initiated fallback (no RelayState). Only honored when the provider was
		// built with allow_idp_initiated; otherwise the response below is rejected
		// because its InResponseTo matches none of the (empty) possible request IDs.
		if provider == nil {
			for name, p := range h.samlProviders {
				provider, idpName = p, name
				break
			}
		}
		if provider == nil {
			fail("provider_not_configured", "No SAML IdP configured.")
			return
		}

		info, err := provider.ValidateResponse(c.Request, possibleRequestIDs, h.cfg.Auth.SAML.GroupAttributeName)
		if err != nil {
			slog.Warn("saml: assertion validation failed", "idp", idpName, "error", err)
			fail("assertion_invalid", "SAML assertion validation failed.")
			return
		}
		if info.Email == "" && info.NameID == "" {
			fail("user_info_failed", "SAML assertion did not contain an email or NameID.")
			return
		}

		// Stable identity for a SAML user is its NameID, namespaced per IdP.
		sub := fmt.Sprintf("saml:%s:%s", idpName, info.NameID)
		if err := h.guardEmailRebind(ctx, sub, info.Email); err != nil {
			fail("email_bound", err.Error())
			return
		}
		user, err := h.userRepo.GetOrCreateUserByOIDC(ctx, sub, info.Email, info.Name)
		if err != nil {
			fail("user_creation_failed", "Failed to look up or create your account.")
			return
		}

		// Reconcile memberships from the SAML group attribute using the shared
		// (deprovisioning) reconciler — same semantics as OIDC/LDAP group mapping.
		desired, managed := saml.ResolveSAMLGroupMappings(info.Groups, h.cfg.Auth.SAML.GroupMappings)
		if mapErr := h.reconcileManagedMemberships(ctx, user.ID, desired, managed, h.cfg.Auth.SAML.DefaultRole); mapErr != nil {
			slog.Warn("failed to apply SAML group mappings", "user_id", user.ID, "error", mapErr)
		}

		scopes, err := h.orgRepo.GetUserCombinedScopes(ctx, user.ID)
		if err != nil {
			scopes = []string{}
		}
		token, err := auth.GenerateJWT(user.ID, user.Email, scopes, sessionTTL)
		if err != nil {
			fail("jwt_failed", "Failed to generate an authentication token.")
			return
		}
		// Attribute the entry to the just-authenticated user: the ACS is an
		// unauthenticated route, so the auth middleware has not set user_id.
		c.Set("user_id", user.ID)
		h.audit.write(c, "auth.login", "user", user.ID,
			map[string]interface{}{"provider": "saml", "idp": idpName, "email": user.Email})
		h.setSessionCookies(c, token)
		c.Redirect(http.StatusFound, frontendBase+"/auth/callback")
	}
}
