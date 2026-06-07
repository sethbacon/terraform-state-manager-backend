// oidc_handlers.go implements admin handlers for reading the active OIDC
// configuration and updating its group-to-role mapping at runtime (without
// re-running the setup wizard). Mirrors the registry's identity surface 1:1.
package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// OIDCConfigAdminHandlers handles admin OIDC configuration endpoints.
type OIDCConfigAdminHandlers struct {
	oidcConfigRepo *repositories.OIDCConfigRepository
}

// NewOIDCConfigAdminHandlers creates a new OIDCConfigAdminHandlers instance.
func NewOIDCConfigAdminHandlers(oidcConfigRepo *repositories.OIDCConfigRepository) *OIDCConfigAdminHandlers {
	return &OIDCConfigAdminHandlers{oidcConfigRepo: oidcConfigRepo}
}

// GetActiveOIDCConfig returns the currently active OIDC configuration, including
// group mapping settings. The client secret is never returned.
// @Summary      Get active OIDC configuration
// @Description  Returns the currently active OIDC configuration including group mapping settings. Client secret is never returned. Requires admin scope.
// @Tags         OIDC
// @Produce      json
// @Success      200  {object}  models.OIDCConfigResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /admin/oidc/config [get]
func (h *OIDCConfigAdminHandlers) GetActiveOIDCConfig(c *gin.Context) {
	ctx := c.Request.Context()

	cfg, err := h.oidcConfigRepo.GetActiveOIDCConfig(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve OIDC configuration"})
		return
	}
	if cfg == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No active OIDC configuration"})
		return
	}

	c.JSON(http.StatusOK, models.OIDCConfigToResponse(cfg))
}

// UpdateGroupMapping updates the group claim name, group-to-role mappings, and
// default role for the active OIDC configuration. Takes effect on next login.
// @Summary      Update OIDC group mapping settings
// @Description  Updates the group claim name, group-to-role mappings, and default role for the active OIDC configuration. Takes effect on the next login. Requires admin scope.
// @Tags         OIDC
// @Accept       json
// @Produce      json
// @Param        body  body  models.OIDCGroupMappingInput  true  "Group mapping configuration"
// @Success      200  {object}  models.OIDCConfigResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /admin/oidc/group-mapping [put]
func (h *OIDCConfigAdminHandlers) UpdateGroupMapping(c *gin.Context) {
	ctx := c.Request.Context()

	var input models.OIDCGroupMappingInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	cfg, err := h.oidcConfigRepo.GetActiveOIDCConfig(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve OIDC configuration"})
		return
	}
	if cfg == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "No active OIDC configuration"})
		return
	}

	if err := cfg.SetGroupMappingConfig(input.GroupClaimName, models.ToIdentityGroupMappings(input.GroupMappings), input.DefaultRole); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encode group mapping"})
		return
	}

	if err := h.oidcConfigRepo.UpdateOIDCConfigExtraConfig(ctx, cfg.ID, cfg.ExtraConfig); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save group mapping"})
		return
	}

	updated, err := h.oidcConfigRepo.GetActiveOIDCConfig(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve updated configuration"})
		return
	}

	c.JSON(http.StatusOK, models.OIDCConfigToResponse(updated))
}
