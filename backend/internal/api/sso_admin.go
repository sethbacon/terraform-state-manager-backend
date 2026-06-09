package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
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
