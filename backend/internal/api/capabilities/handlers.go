// Package capabilities implements the capability discovery endpoint. It exposes
// the set of capabilities registered at startup (internal/capability) so clients
// can see which extension features — and their scheduled task types and RBAC
// scopes — are available on this deployment.
package capabilities

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/capability"
)

// Handlers serves the capability discovery endpoint.
type Handlers struct {
	registry *capability.Registry
}

// NewHandlers constructs capability discovery Handlers backed by the given
// registry. registry may be nil, in which case the endpoint reports an empty set.
func NewHandlers(registry *capability.Registry) *Handlers {
	return &Handlers{registry: registry}
}

// capabilityView is the public projection of a registered capability. It omits
// the handler and route registrar (which are not serializable) and exposes only
// the discovery metadata.
type capabilityView struct {
	Name     string   `json:"name"`
	Key      string   `json:"key"`
	TaskType string   `json:"task_type,omitempty"`
	Scopes   []string `json:"scopes"`
}

// ListCapabilities handles GET /api/v1/capabilities.
//
// @Summary      List registered capabilities
// @Description  Returns the capabilities registered at startup, including each capability's scheduled task type (if any) and the RBAC scopes it introduces.
// @Tags         Capabilities
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /capabilities [get]
func (h *Handlers) ListCapabilities(c *gin.Context) {
	views := make([]capabilityView, 0)
	if h.registry != nil {
		for _, cap := range h.registry.List() {
			scopes := cap.Scopes
			if scopes == nil {
				scopes = []string{}
			}
			views = append(views, capabilityView{
				Name:     cap.Name,
				Key:      cap.Key,
				TaskType: cap.TaskType,
				Scopes:   scopes,
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"data":  views,
		"total": len(views),
	})
}
