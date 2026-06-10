package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// SSOConfigHandler returns a read-only, admin-scoped view of the configured SSO /
// identity providers (OIDC, SAML, LDAP, mTLS) and the SCIM provisioning toggle —
// enough for the admin UI to show what is wired up and the group→role mappings.
//
// SECURITY: secrets are never returned (no client secret, LDAP bind password, or
// SP/CA key material) — only non-sensitive config and the admin-authored mappings.
// @Summary      SSO / identity configuration
// @Description  Read-only view of configured identity providers (OIDC, SAML, LDAP, mTLS) and the SCIM toggle, including group-to-role mappings. Secrets are omitted.
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/sso [get]
func (h *AuthHandlers) SSOConfigHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		oidc := h.cfg.Auth.OIDC
		oidcMappings := make([]gin.H, 0, len(oidc.GroupMappings))
		for _, m := range oidc.GroupMappings {
			oidcMappings = append(oidcMappings, gin.H{"group": m.Group, "organization": m.Organization, "role": m.Role})
		}

		saml := h.cfg.Auth.SAML
		samlMappings := make([]gin.H, 0, len(saml.GroupMappings))
		for _, m := range saml.GroupMappings {
			samlMappings = append(samlMappings, gin.H{"group": m.Group, "organization": m.Organization, "role": m.Role})
		}
		// IdP names come from the live providers map so the list reflects what
		// actually initialised; sorted for a stable response.
		samlIdPs := make([]string, 0, len(h.samlProviders))
		for name := range h.samlProviders {
			samlIdPs = append(samlIdPs, name)
		}
		sort.Strings(samlIdPs)

		ldap := h.cfg.Auth.LDAP
		ldapMappings := make([]gin.H, 0, len(ldap.GroupMappings))
		for _, m := range ldap.GroupMappings {
			ldapMappings = append(ldapMappings, gin.H{"group_dn": m.GroupDN, "organization": m.Organization, "role": m.Role})
		}

		mtls := h.cfg.Auth.MTLS
		mtlsMappings := make([]gin.H, 0, len(mtls.Mappings))
		for _, m := range mtls.Mappings {
			mtlsMappings = append(mtlsMappings, gin.H{"subject": m.Subject, "scopes": m.Scopes})
		}

		c.JSON(http.StatusOK, gin.H{
			"oidc": gin.H{
				"enabled":          oidc.Enabled,
				"issuer_url":       oidc.IssuerURL,
				"group_claim_name": oidc.GroupClaimName,
				"default_role":     oidc.DefaultRole,
				"group_mappings":   oidcMappings,
			},
			"saml": gin.H{
				"enabled":              saml.Enabled,
				"entity_id":            saml.EntityID,
				"acs_url":              saml.ACSURL,
				"allow_idp_initiated":  saml.AllowIDPInitiated,
				"group_attribute_name": saml.GroupAttributeName,
				"default_role":         saml.DefaultRole,
				"idps":                 samlIdPs,
				"group_mappings":       samlMappings,
			},
			"ldap": gin.H{
				"enabled":        ldap.Enabled,
				"host":           ldap.Host,
				"use_tls":        ldap.UseTLS,
				"start_tls":      ldap.StartTLS,
				"base_dn":        ldap.BaseDN,
				"default_role":   ldap.DefaultRole,
				"group_mappings": ldapMappings,
			},
			"mtls": gin.H{
				"enabled":        mtls.Enabled,
				"client_ca_file": mtls.ClientCAFile,
				"mappings":       mtlsMappings,
			},
			"scim": gin.H{
				"enabled": h.cfg.Auth.SCIM.Enabled,
			},
		})
	}
}

// oidcConfigResponse renders the effective OIDC group-mapping configuration:
// provider identity from the file config, group settings from the admin-saved
// overlay when present (mirrors the registry's OIDCConfigResponse shape).
func (h *AuthHandlers) oidcConfigResponse(c *gin.Context) gin.H {
	claimName, mappings, defaultRole := h.effectiveOIDCGroupConfig(c.Request.Context())
	out := make([]gin.H, 0, len(mappings))
	for _, m := range mappings {
		out = append(out, gin.H{"group": m.Group, "organization": m.Organization, "role": m.Role})
	}
	return gin.H{
		"provider_type":    "oidc",
		"issuer_url":       h.cfg.Auth.OIDC.IssuerURL,
		"client_id":        h.cfg.Auth.OIDC.ClientID,
		"is_active":        h.cfg.Auth.OIDC.Enabled,
		"group_claim_name": claimName,
		"default_role":     defaultRole,
		"group_mappings":   out,
	}
}

// OIDCConfigHandler returns the active OIDC configuration including the
// effective group-mapping settings. The client secret is never returned.
// @Summary      Get active OIDC configuration
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/oidc/config [get]
func (h *AuthHandlers) OIDCConfigHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, h.oidcConfigResponse(c))
	}
}

type oidcGroupMappingInput struct {
	GroupClaimName string `json:"group_claim_name"`
	DefaultRole    string `json:"default_role"`
	GroupMappings  []struct {
		Group        string `json:"group"`
		Organization string `json:"organization"`
		Role         string `json:"role"`
	} `json:"group_mappings"`
}

// UpdateOIDCGroupMapping saves the runtime group-mapping overlay (group claim
// name, group→org/role mappings, default role). Takes effect on the next login.
// @Summary      Update OIDC group mapping settings
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/oidc/group-mapping [put]
func (h *AuthHandlers) UpdateOIDCGroupMapping() gin.HandlerFunc {
	return func(c *gin.Context) {
		var input oidcGroupMappingInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		settings := &repositories.SSOSettings{
			OIDCGroupClaimName: strings.TrimSpace(input.GroupClaimName),
			OIDCDefaultRole:    strings.TrimSpace(input.DefaultRole),
			OIDCGroupMappings:  make([]repositories.SSOGroupMapping, 0, len(input.GroupMappings)),
		}
		for _, m := range input.GroupMappings {
			g, o, r := strings.TrimSpace(m.Group), strings.TrimSpace(m.Organization), strings.TrimSpace(m.Role)
			if g == "" || o == "" || r == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "each mapping requires group, organization, and role"})
				return
			}
			settings.OIDCGroupMappings = append(settings.OIDCGroupMappings,
				repositories.SSOGroupMapping{Group: g, Organization: o, Role: r})
		}
		if err := h.ssoSettings.Upsert(c.Request.Context(), settings); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save group mapping"})
			return
		}
		c.JSON(http.StatusOK, h.oidcConfigResponse(c))
	}
}

// IdentityGroupMappingsHandler returns the SAML and LDAP group mappings from the
// server configuration (read-only — these are file-configured).
// @Summary      SAML/LDAP group mappings
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/identity-group-mappings [get]
func (h *AuthHandlers) IdentityGroupMappingsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		resp := gin.H{}
		if saml := h.cfg.Auth.SAML; saml.Enabled {
			mappings := make([]gin.H, 0, len(saml.GroupMappings))
			for _, m := range saml.GroupMappings {
				mappings = append(mappings, gin.H{"group": m.Group, "organization": m.Organization, "role": m.Role})
			}
			resp["saml"] = gin.H{
				"group_attribute_name": saml.GroupAttributeName,
				"default_role":         saml.DefaultRole,
				"group_mappings":       mappings,
			}
		}
		if ldap := h.cfg.Auth.LDAP; ldap.Enabled {
			mappings := make([]gin.H, 0, len(ldap.GroupMappings))
			for _, m := range ldap.GroupMappings {
				mappings = append(mappings, gin.H{"group_dn": m.GroupDN, "organization": m.Organization, "role": m.Role})
			}
			resp["ldap"] = gin.H{
				"default_role":   ldap.DefaultRole,
				"group_mappings": mappings,
			}
		}
		c.JSON(http.StatusOK, resp)
	}
}

// MTLSConfigHandler returns the mTLS client-certificate configuration (read-only
// — certificate-subject mappings are file-configured). No key material is returned.
// @Summary      mTLS configuration
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/mtls [get]
func (h *AuthHandlers) MTLSConfigHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		mtls := h.cfg.Auth.MTLS
		mappings := make([]gin.H, 0, len(mtls.Mappings))
		for _, m := range mtls.Mappings {
			mappings = append(mappings, gin.H{"subject": m.Subject, "scopes": m.Scopes})
		}
		c.JSON(http.StatusOK, gin.H{
			"enabled":        mtls.Enabled,
			"client_ca_file": mtls.ClientCAFile,
			"mappings":       mappings,
		})
	}
}
