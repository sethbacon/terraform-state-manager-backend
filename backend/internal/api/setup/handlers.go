// Package setup implements the first-run setup wizard handlers for the
// Terraform State Manager. The wizard walks the operator through OIDC
// configuration, admin user creation and final activation.
package setup

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/oidc"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// OIDCConfigInput carries the user-supplied OIDC provider settings submitted
// during the setup wizard.
type OIDCConfigInput struct {
	Name         string   `json:"name"`
	ProviderType string   `json:"provider_type"`
	IssuerURL    string   `json:"issuer_url" binding:"required"`
	ClientID     string   `json:"client_id" binding:"required"`
	ClientSecret string   `json:"client_secret" binding:"required"`
	RedirectURL  string   `json:"redirect_url" binding:"required"`
	Scopes       []string `json:"scopes"`
}

// AdminConfigInput carries the initial admin user details.
type AdminConfigInput struct {
	Email string `json:"email" binding:"required"`
	Name  string `json:"name" binding:"required"`
}

// Handlers exposes the setup wizard HTTP handlers. A callback function is used
// to push a newly created OIDCProvider into the running AuthHandlers instance
// so that OIDC login becomes available without a server restart.
type Handlers struct {
	cfg             *config.Config
	db              *sql.DB
	tokenCipher     *crypto.TokenCipher
	oidcConfigRepo  *repositories.OIDCConfigRepository
	userRepo        *repositories.UserRepository
	orgRepo         *repositories.OrganizationRepository
	setOIDCProvider func(*oidc.OIDCProvider)
}

// NewHandlers creates a new setup Handlers instance. The setOIDCProviderFunc
// callback is invoked after a successful OIDC configuration save to activate
// the provider at runtime (typically wired to AuthHandlers.SetOIDCProvider).
func NewHandlers(
	cfg *config.Config,
	db *sql.DB,
	tokenCipher *crypto.TokenCipher,
	oidcConfigRepo *repositories.OIDCConfigRepository,
	userRepo *repositories.UserRepository,
	orgRepo *repositories.OrganizationRepository,
	setOIDCProviderFunc func(*oidc.OIDCProvider),
) *Handlers {
	return &Handlers{
		cfg:             cfg,
		db:              db,
		tokenCipher:     tokenCipher,
		oidcConfigRepo:  oidcConfigRepo,
		userRepo:        userRepo,
		orgRepo:         orgRepo,
		setOIDCProvider: setOIDCProviderFunc,
	}
}

// ---------------------------------------------------------------------------
// Handler methods
// ---------------------------------------------------------------------------

// GetSetupStatus returns the current state of the setup wizard. It is the only
// setup endpoint that does not require the setup token, allowing the frontend
// to decide whether to show the wizard or the normal login screen.
// GetSetupStatus godoc
// @Summary      Get setup status
// @Tags         Setup
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /setup/status [get]
func (h *Handlers) GetSetupStatus(c *gin.Context) {
	ctx := c.Request.Context()

	setupCompleted, err := h.oidcConfigRepo.IsSetupCompleted(ctx)
	if err != nil {
		slog.Error("Failed to check setup completion status", "error", err)
	}

	oidcConfigured := false
	if _, err := h.oidcConfigRepo.GetActiveOIDCConfig(ctx); err == nil {
		oidcConfigured = true
	}

	adminConfigured := false
	if _, total, err := h.userRepo.ListUsers(ctx, 0, 1); err == nil && total > 0 {
		adminConfigured = true
	}

	c.JSON(http.StatusOK, gin.H{
		"setup_completed":  setupCompleted,
		"oidc_configured":  oidcConfigured,
		"admin_configured": adminConfigured,
		"setup_required":   !setupCompleted,
	})
}

// ValidateToken confirms that the setup token supplied via the
// SetupTokenMiddleware is valid. If the request reaches this handler, the
// middleware has already accepted the token.
// ValidateToken godoc
// @Summary      Validate setup token
// @Tags         Setup
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Router       /setup/validate-token [post]
func (h *Handlers) ValidateToken(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"valid": true})
}

// TestOIDCConfig validates an OIDC configuration by performing provider
// discovery against the issuer URL. The discovery request is capped at 15
// seconds so the wizard does not hang on unreachable endpoints.
// TestOIDCConfig godoc
// @Summary      Test OIDC configuration
// @Tags         Setup
// @Accept       json
// @Produce      json
// @Param        input  body  OIDCConfigInput  true  "OIDC configuration"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Router       /setup/oidc/test [post]
func (h *Handlers) TestOIDCConfig(c *gin.Context) {
	var input OIDCConfigInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	scopes := input.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}

	oidcCfg := &config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    input.IssuerURL,
		ClientID:     input.ClientID,
		ClientSecret: input.ClientSecret,
		RedirectURL:  input.RedirectURL,
		Scopes:       scopes,
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	if _, err := oidc.NewOIDCProviderWithContext(ctx, oidcCfg); err != nil {
		slog.Warn("OIDC configuration test failed",
			"issuer", input.IssuerURL, "error", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "OIDC configuration test failed",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "OIDC provider discovery successful",
	})
}

// SaveOIDCConfig encrypts the client secret, persists the OIDC configuration
// to the database, and activates a live OIDC provider so that login is usable
// immediately.
// SaveOIDCConfig godoc
// @Summary      Save OIDC configuration
// @Tags         Setup
// @Accept       json
// @Produce      json
// @Param        input  body  OIDCConfigInput  true  "OIDC configuration"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /setup/oidc [post]
func (h *Handlers) SaveOIDCConfig(c *gin.Context) {
	var input OIDCConfigInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	// Encrypt the client secret before storing it.
	encryptedSecret, err := h.tokenCipher.Seal(input.ClientSecret)
	if err != nil {
		slog.Error("Failed to encrypt OIDC client secret", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt client secret"})
		return
	}

	scopes := input.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}

	oidcConfig := &models.OIDCConfig{
		IssuerURL:             input.IssuerURL,
		ClientID:              input.ClientID,
		ClientSecretEncrypted: encryptedSecret,
		RedirectURL:           input.RedirectURL,
		ScopesJSON:            strings.Join(scopes, ","),
		IsActive:              true,
	}

	ctx := c.Request.Context()

	if err := h.oidcConfigRepo.SaveOIDCConfig(ctx, oidcConfig); err != nil {
		slog.Error("Failed to save OIDC config", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save OIDC configuration"})
		return
	}

	// Build a live OIDC provider from the plain-text credentials and push
	// it into the auth handlers so that /auth/login works immediately.
	liveCfg := &config.OIDCConfig{
		Enabled:      true,
		IssuerURL:    input.IssuerURL,
		ClientID:     input.ClientID,
		ClientSecret: input.ClientSecret,
		RedirectURL:  input.RedirectURL,
		Scopes:       scopes,
	}

	provider, err := oidc.NewOIDCProvider(liveCfg)
	if err != nil {
		slog.Error("OIDC config saved but provider activation failed", "error", err)
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"warning": "OIDC configuration saved but provider activation failed: " + err.Error(),
		})
		return
	}

	if h.setOIDCProvider != nil {
		h.setOIDCProvider(provider)
	}

	slog.Info("OIDC configuration saved and provider activated",
		"issuer", input.IssuerURL)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "OIDC configuration saved and activated",
	})
}

// ConfigureAdmin creates the initial admin user, ensures the default
// organisation exists, and grants the user the admin role template.
// ConfigureAdmin godoc
// @Summary      Configure admin user
// @Tags         Setup
// @Accept       json
// @Produce      json
// @Param        input  body  AdminConfigInput  true  "Admin configuration"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /setup/admin [post]
func (h *Handlers) ConfigureAdmin(c *gin.Context) {
	var input AdminConfigInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	ctx := c.Request.Context()

	// Guard against duplicate creation.
	existing, err := h.userRepo.GetUserByEmail(ctx, input.Email)
	if err != nil {
		slog.Error("Failed to check for existing user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check existing user"})
		return
	}
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "user already exists",
			"user_id": existing.ID,
		})
		return
	}

	// Create the admin user.
	user := &models.User{
		Email:    input.Email,
		Name:     input.Name,
		IsActive: true,
	}
	if err := h.userRepo.CreateUser(ctx, user); err != nil {
		slog.Error("Failed to create admin user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create admin user"})
		return
	}

	// Ensure the default organisation exists.
	defaultOrgName := h.cfg.MultiTenancy.DefaultOrganization
	if defaultOrgName == "" {
		defaultOrgName = "default"
	}

	org, err := h.orgRepo.GetOrganizationByName(ctx, defaultOrgName)
	if err != nil {
		slog.Warn("Error looking up default organisation", "org_name", defaultOrgName, "error", err)
	}
	if org == nil {
		displayName := defaultOrgName
		if len(displayName) > 0 {
			displayName = strings.ToUpper(displayName[:1]) + displayName[1:]
		}
		org = &models.Organization{
			Name:        defaultOrgName,
			DisplayName: displayName,
			IsActive:    true,
		}
		if err := h.orgRepo.CreateOrganization(ctx, org); err != nil {
			slog.Error("Failed to create default organisation", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create default organisation"})
			return
		}
	}

	// Resolve the admin role template.
	var adminRoleID *string
	var rtID string
	err = h.db.QueryRowContext(ctx,
		"SELECT id FROM role_templates WHERE name = 'admin' LIMIT 1",
	).Scan(&rtID)
	if err == nil {
		adminRoleID = &rtID
	} else {
		slog.Warn("Admin role template not found; creating membership without explicit role",
			"error", err)
	}

	// Add the user to the default organisation with the admin role.
	member := &models.OrganizationMember{
		OrganizationID: org.ID,
		UserID:         user.ID,
		RoleTemplateID: adminRoleID,
	}
	if err := h.orgRepo.AddMember(ctx, member); err != nil {
		slog.Error("Failed to add admin to organisation", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add admin to organisation"})
		return
	}

	slog.Info("Admin user configured",
		"user_id", user.ID, "email", user.Email, "org_id", org.ID)

	c.JSON(http.StatusOK, gin.H{
		"success":         true,
		"user_id":         user.ID,
		"organization_id": org.ID,
		"message":         "Admin user created and added to organisation",
	})
}

// CompleteSetup verifies that all required setup components (OIDC and admin)
// are configured and marks the setup as completed. Once completed, the
// SetupTokenMiddleware will reject all further setup requests.
// CompleteSetup godoc
// @Summary      Complete setup
// @Tags         Setup
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /setup/complete [post]
func (h *Handlers) CompleteSetup(c *gin.Context) {
	ctx := c.Request.Context()

	// Verify OIDC is configured.
	if _, err := h.oidcConfigRepo.GetActiveOIDCConfig(ctx); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "OIDC not configured",
			"message": "Please configure OIDC authentication before completing setup.",
		})
		return
	}

	// Verify at least one admin user exists.
	if _, total, err := h.userRepo.ListUsers(ctx, 0, 1); err != nil || total == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "admin not configured",
			"message": "Please configure an admin user before completing setup.",
		})
		return
	}

	// Mark setup as completed.
	if err := h.oidcConfigRepo.SetSetupCompleted(ctx); err != nil {
		slog.Error("Failed to mark setup as completed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete setup"})
		return
	}

	slog.Info("Setup wizard completed successfully")

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Setup completed successfully. The application is now ready to use.",
	})
}
