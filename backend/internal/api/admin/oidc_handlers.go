// oidc_handlers.go provides admin HTTP handlers for reading and updating the
// OIDC provider configuration stored in the database.
package admin

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// OIDCConfigResponse is the API response shape for a GET /admin/oidc request.
// The client_secret is always masked as "***" so it is never sent in plain text.
type OIDCConfigResponse struct {
	IssuerURL    string   `json:"issuer_url"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"` // always "***"
	RedirectURL  string   `json:"redirect_url"`
	Scopes       []string `json:"scopes"`
	Enabled      bool     `json:"enabled"`
}

// UpdateOIDCConfigRequest is the request body for PUT /admin/oidc.
// Sending "***" or an empty string for client_secret preserves the existing
// encrypted secret so the caller does not need to re-supply it.
type UpdateOIDCConfigRequest struct {
	IssuerURL    string   `json:"issuer_url" binding:"required"`
	ClientID     string   `json:"client_id" binding:"required"`
	ClientSecret string   `json:"client_secret"`
	RedirectURL  string   `json:"redirect_url"`
	Scopes       []string `json:"scopes"`
	Enabled      bool     `json:"enabled"`
}

// TestOIDCConfigRequest is the request body for POST /admin/oidc/test.
type TestOIDCConfigRequest struct {
	IssuerURL string `json:"issuer_url" binding:"required"`
	ClientID  string `json:"client_id" binding:"required"`
}

// OIDCHandlers provides HTTP handlers for admin OIDC configuration management.
type OIDCHandlers struct {
	tokenCipher    *crypto.TokenCipher
	oidcConfigRepo *repositories.OIDCConfigRepository
}

// NewOIDCHandlers constructs an OIDCHandlers with the given cipher and repository.
func NewOIDCHandlers(tokenCipher *crypto.TokenCipher, oidcConfigRepo *repositories.OIDCConfigRepository) *OIDCHandlers {
	return &OIDCHandlers{
		tokenCipher:    tokenCipher,
		oidcConfigRepo: oidcConfigRepo,
	}
}

// GetOIDCConfig returns the current active OIDC configuration.
// The client_secret field is always returned as "***".
// GET /api/v1/admin/oidc
func (h *OIDCHandlers) GetOIDCConfig(c *gin.Context) {
	ctx := c.Request.Context()

	cfg, err := h.oidcConfigRepo.GetActiveOIDCConfig(ctx)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no OIDC configuration found"})
		return
	}

	resp := OIDCConfigResponse{
		IssuerURL:    cfg.IssuerURL,
		ClientID:     cfg.ClientID,
		ClientSecret: "***",
		RedirectURL:  cfg.RedirectURL,
		Scopes:       cfg.GetScopes(),
		Enabled:      cfg.IsActive,
	}
	c.JSON(http.StatusOK, resp)
}

// UpdateOIDCConfig saves a new OIDC configuration to the database.
// If client_secret is empty or "***", the existing encrypted secret is reused.
// PUT /api/v1/admin/oidc
func (h *OIDCHandlers) UpdateOIDCConfig(c *gin.Context) {
	var req UpdateOIDCConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	var encryptedSecret string

	// Preserve existing encrypted secret when the caller sends the masked placeholder.
	if req.ClientSecret == "" || req.ClientSecret == "***" {
		existing, err := h.oidcConfigRepo.GetActiveOIDCConfig(ctx)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "client_secret is required when no existing OIDC configuration is present",
			})
			return
		}
		encryptedSecret = existing.ClientSecretEncrypted
	} else {
		encrypted, err := h.tokenCipher.Seal(req.ClientSecret)
		if err != nil {
			slog.Error("failed to encrypt OIDC client secret", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt client secret"})
			return
		}
		encryptedSecret = encrypted
	}

	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}

	oidcConfig := &models.OIDCConfig{
		IssuerURL:             req.IssuerURL,
		ClientID:              req.ClientID,
		ClientSecretEncrypted: encryptedSecret,
		RedirectURL:           req.RedirectURL,
		ScopesJSON:            strings.Join(scopes, ","),
		IsActive:              true,
	}

	if err := h.oidcConfigRepo.SaveOIDCConfig(ctx, oidcConfig); err != nil {
		slog.Error("failed to save OIDC config", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save OIDC configuration"})
		return
	}

	slog.Info("OIDC configuration updated via admin API", "issuer", req.IssuerURL)
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "OIDC configuration saved successfully"})
}

// TestOIDCConfig verifies reachability of an OIDC provider by issuing a HEAD
// request to the well-known discovery endpoint.
// POST /api/v1/admin/oidc/test
func (h *OIDCHandlers) TestOIDCConfig(c *gin.Context) {
	var req TestOIDCConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	discoveryURL := strings.TrimRight(req.IssuerURL, "/") + "/.well-known/openid-configuration"

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Head(discoveryURL)
	if err != nil {
		slog.Warn("OIDC provider connectivity test failed", "issuer", req.IssuerURL, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{
			"success": false,
			"error":   "failed to reach OIDC provider: " + err.Error(),
		})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		c.JSON(http.StatusBadGateway, gin.H{
			"success": false,
			"error":   fmt.Sprintf("OIDC provider returned unexpected status %d", resp.StatusCode),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "OIDC provider is reachable",
	})
}
