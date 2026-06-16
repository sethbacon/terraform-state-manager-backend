package setup

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// oidcDiscoveryTimeout bounds the call to the issuer's discovery document so a
// slow/unreachable IdP can't hang the setup request.
const oidcDiscoveryTimeout = 8 * time.Second

type oidcRequest struct {
	IssuerURL    string   `json:"issuer_url" binding:"required"`
	ClientID     string   `json:"client_id" binding:"required"`
	ClientSecret string   `json:"client_secret" binding:"required"`
	RedirectURL  string   `json:"redirect_url"`
	Scopes       []string `json:"scopes"`
}

func (h *Handlers) defaultRedirectURL() string {
	base := h.cfg.Server.PublicURL
	if base == "" {
		base = h.cfg.Server.BaseURL
	}
	return strings.TrimRight(base, "/") + "/api/v1/auth/callback"
}

func (req oidcRequest) toConfig(defaultRedirect string) *config.OIDCConfig {
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	redirect := req.RedirectURL
	if redirect == "" {
		redirect = defaultRedirect
	}
	return &config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    req.IssuerURL,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		RedirectURL:  redirect,
		Scopes:       scopes,
	}
}

// TestOIDCConfig validates connectivity to the issuer without persisting:
// constructing the provider performs OIDC discovery against the issuer, so a
// successful build IS the test.
func (h *Handlers) TestOIDCConfig(c *gin.Context) {
	var req oidcRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "issuer_url, client_id, and client_secret are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), oidcDiscoveryTimeout)
	defer cancel()
	if _, err := auth.NewOIDCProviderWithContext(ctx, req.toConfig(h.defaultRedirectURL())); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"status": "failed", "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// SaveOIDCConfig validates, persists (client secret encrypted), records that
// OIDC is configured, and activates the provider in the live auth handler so
// the owner can log in without a restart.
func (h *Handlers) SaveOIDCConfig(c *gin.Context) {
	// Defense in depth: refuse to configure identity when the sibling owns it
	// (coupled). The wizard already hides this step, but a hand-crafted request
	// must not clobber the shared identity store.
	if !h.cfg.Suite.ShouldSeedRoles("tsm") {
		c.JSON(http.StatusConflict, gin.H{"error": "identity is managed by the suite registry; configure OIDC there"})
		return
	}
	if !crypto.Available() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store the OIDC client secret: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
		return
	}
	var req oidcRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "issuer_url, client_id, and client_secret are required"})
		return
	}
	cfg := req.toConfig(h.defaultRedirectURL())

	ctx, cancel := context.WithTimeout(c.Request.Context(), oidcDiscoveryTimeout)
	defer cancel()
	provider, err := auth.NewOIDCProviderWithContext(ctx, cfg)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "OIDC discovery failed: " + err.Error()})
		return
	}

	enc, err := crypto.Encrypt([]byte(req.ClientSecret))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt client secret"})
		return
	}
	if err := h.oidc.Create(c.Request.Context(), repositories.OIDCConfig{
		IssuerURL:             cfg.IssuerURL,
		ClientID:              cfg.ClientID,
		ClientSecretEncrypted: enc,
		RedirectURL:           cfg.RedirectURL,
		Scopes:                cfg.Scopes,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save OIDC configuration"})
		return
	}
	if err := h.settings.SetOIDCConfigured(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record OIDC status"})
		return
	}
	if h.activateOIDC != nil {
		h.activateOIDC(provider)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
