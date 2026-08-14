// Package setup serves TSM's first-run setup-wizard endpoints. The status
// endpoint is public so the SPA can decide whether to show the wizard; all
// mutating endpoints sit behind middleware.SetupTokenMiddleware and are
// permanently disabled once setup completes.
package setup

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// Handlers serves the setup-wizard endpoints.
type Handlers struct {
	settings *repositories.SystemSettingsRepository
	oidc     *repositories.OIDCConfigRepository
	sources  *repositories.SourceRepository
	// identityDB backs the owner step's user/membership writes; the idstore repos
	// are built lazily in ConfigureAdmin so a nil-DB construction never panics.
	identityDB *sql.DB
	// platformAdmins is the carrier the owner step writes the FIRST platform-admin
	// grant into. Nil where there is no carrier (the nil-DB rigs), in which case
	// the owner step still creates the user and the role assignment.
	platformAdmins *platformadmin.Service
	cfg            *config.Config
	// activateOIDC swaps the live auth handler's OIDC provider at runtime. It is
	// injected by the router (the api package owns AuthHandlers, and setup must
	// not import api — that would be an import cycle).
	activateOIDC func(*auth.OIDCProvider)
}

// NewHandlers constructs the setup handlers. Dependencies may be nil in contexts
// that never serve the corresponding step (e.g. status-only tests).
func NewHandlers(
	settings *repositories.SystemSettingsRepository,
	oidc *repositories.OIDCConfigRepository,
	sources *repositories.SourceRepository,
	identityDB *sql.DB,
	platformAdmins *platformadmin.Service,
	cfg *config.Config,
	activateOIDC func(*auth.OIDCProvider),
) *Handlers {
	return &Handlers{
		settings:       settings,
		oidc:           oidc,
		sources:        sources,
		identityDB:     identityDB,
		platformAdmins: platformAdmins,
		cfg:            cfg,
		activateOIDC:   activateOIDC,
	}
}

// GetSetupStatus reports first-run setup state (public — no auth).
//
// identity_owned_externally is driven by the SYNCHRONOUS role_seed_owner config
// (!ShouldSeedRoles("tsm")), NOT live sibling discovery. Discovery is
// StateUnknown at first boot (it only flips after the first poll), so a
// discovery-based signal would briefly expose the identity steps and let an
// operator clobber the shared identity in a coupled deployment. The config flag
// is known at boot and is the same guard bootstrap.Run already uses.
func (h *Handlers) GetSetupStatus(c *gin.Context) {
	ctx := c.Request.Context()
	st, err := h.settings.GetStatus(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load setup status"})
		return
	}
	pending, err := h.settings.HasPendingFeatureSetup(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load setup status"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"setup_required":            !st.SetupCompleted,
		"setup_completed":           st.SetupCompleted,
		"pending_feature_setup":     pending,
		"auth_method":               st.AuthMethod,
		"admin_configured":          st.AdminConfigured,
		"oidc_configured":           st.OIDCConfigured,
		"ldap_configured":           st.LDAPConfigured,
		"sources_configured":        st.SourcesConfigured,
		"identity_owned_externally": !h.cfg.Suite.ShouldSeedRoles("tsm"),
	})
}

// ValidateToken confirms the setup token. The middleware has already validated
// it, so reaching here means the token is good and the wizard may proceed.
func (h *Handlers) ValidateToken(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"valid": true})
}
