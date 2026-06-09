package sources

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/repolink"
)

// RepoLinkHandlers serves the source-to-ADO repo-link sub-resource
// (GET|PUT|DELETE /api/v1/sources/{id}/repo-link) plus the auto-discover
// endpoint (POST /api/v1/sources/{id}/repo-link/discover).
//
// The repo link binds a state source to the Azure DevOps repo/pipeline that owns
// its Terraform configuration; it is consumed by the outbound drift-trigger and
// repo-metadata analysis. CRUD is the manual path. Discovery is the auto path:
// it is credential-gated and returns 503 until a live ADO client is wired,
// mirroring the inbound drift-ingest unconfigured behaviour.
type RepoLinkHandlers struct {
	sourceRepo   *repositories.StateSourceRepository
	repoLinkRepo *repositories.SourceRepoLinkRepository
	discoverer   repolink.Discoverer
}

// NewRepoLinkHandlers creates a RepoLinkHandlers. discoverer is the auto-discover
// seam; pass repolink.NewStubDiscoverer() when no ADO client is configured (the
// discover endpoint then returns 503).
func NewRepoLinkHandlers(
	sourceRepo *repositories.StateSourceRepository,
	repoLinkRepo *repositories.SourceRepoLinkRepository,
	discoverer repolink.Discoverer,
) *RepoLinkHandlers {
	return &RepoLinkHandlers{
		sourceRepo:   sourceRepo,
		repoLinkRepo: repoLinkRepo,
		discoverer:   discoverer,
	}
}

// GetRepoLink handles GET /api/v1/sources/:id/repo-link.
// Returns the source's ADO repo link, or 404 when the source has none.
// @Summary      Get a source's repo link
// @Description  Returns the Azure DevOps repo/pipeline link for the state source, or 404 when no link is set
// @Tags         Sources
// @Produce      json
// @Param        id  path      string  true  "Source ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /sources/{id}/repo-link [get]
func (h *RepoLinkHandlers) GetRepoLink(c *gin.Context) {
	ctx := c.Request.Context()
	source, ok := h.resolveOwnedSource(c)
	if !ok {
		return
	}

	link, err := h.repoLinkRepo.GetBySourceID(ctx, source.ID)
	if err != nil {
		slog.Error("Failed to get source repo link", "error", err, "source_id", source.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve repo link"})
		return
	}
	if link == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repo link not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": link})
}

// SetRepoLink handles PUT /api/v1/sources/:id/repo-link.
// Creates or replaces the source's ADO repo link (manual discovery method).
// @Summary      Set a source's repo link
// @Description  Creates or replaces the Azure DevOps repo/pipeline link for the state source (manual link)
// @Tags         Sources
// @Accept       json
// @Produce      json
// @Param        id       path      string                         true  "Source ID"
// @Param        request  body      models.SourceRepoLinkRequest   true  "Repo link request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /sources/{id}/repo-link [put]
func (h *RepoLinkHandlers) SetRepoLink(c *gin.Context) {
	ctx := c.Request.Context()
	source, ok := h.resolveOwnedSource(c)
	if !ok {
		return
	}

	var req models.SourceRepoLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}
	if req.ADOPipelineID != nil && *req.ADOPipelineID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ado_pipeline_id must be positive"})
		return
	}

	link := &models.SourceRepoLink{
		OrganizationID:     source.OrganizationID,
		SourceID:           source.ID,
		ADOOrganizationURL: req.ADOOrganizationURL,
		ADOProject:         req.ADOProject,
		ADORepo:            req.ADORepo,
		ADOPipelineID:      req.ADOPipelineID,
		// This endpoint sets links manually; auto-discovery sets this to "auto".
		DiscoveryMethod: models.RepoLinkDiscoveryManual,
	}

	if err := h.repoLinkRepo.Upsert(ctx, link); err != nil {
		slog.Error("Failed to set source repo link", "error", err, "source_id", source.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set repo link"})
		return
	}

	slog.Info("Source repo link set",
		"source_id", source.ID, "ado_repo", link.ADORepo, "ado_project", link.ADOProject)
	c.JSON(http.StatusOK, gin.H{"data": link})
}

// DeleteRepoLink handles DELETE /api/v1/sources/:id/repo-link.
// Removes the source's ADO repo link. Deleting an absent link is a no-op.
// @Summary      Delete a source's repo link
// @Description  Removes the Azure DevOps repo/pipeline link for the state source
// @Tags         Sources
// @Produce      json
// @Param        id  path      string  true  "Source ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /sources/{id}/repo-link [delete]
func (h *RepoLinkHandlers) DeleteRepoLink(c *gin.Context) {
	ctx := c.Request.Context()
	source, ok := h.resolveOwnedSource(c)
	if !ok {
		return
	}

	if err := h.repoLinkRepo.DeleteBySourceID(ctx, source.ID); err != nil {
		slog.Error("Failed to delete source repo link", "error", err, "source_id", source.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete repo link"})
		return
	}

	slog.Info("Source repo link deleted", "source_id", source.ID)
	c.JSON(http.StatusOK, gin.H{"message": "repo link deleted successfully"})
}

// DiscoverRepoLink handles POST /api/v1/sources/:id/repo-link/discover.
// Suggests ADO repo/pipeline candidates for the source by name convention. The
// live path is credential-gated: when no ADO client is configured the endpoint
// returns 503 ("discovery requires ADO configuration"), mirroring the inbound
// drift-ingest endpoint. Discovery never writes a link; an operator confirms a
// candidate via PUT /repo-link.
// @Summary      Auto-discover repo-link candidates
// @Description  Suggests Azure DevOps repo/pipeline candidates for the source by name convention. Returns 503 when ADO auto-discovery is not configured (no ADO client). Candidates are not persisted; confirm one via PUT /sources/{id}/repo-link.
// @Tags         Sources
// @Produce      json
// @Param        id  path      string  true  "Source ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /sources/{id}/repo-link/discover [post]
func (h *RepoLinkHandlers) DiscoverRepoLink(c *gin.Context) {
	ctx := c.Request.Context()

	if h.discoverer == nil || !h.discoverer.Configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "repo-link auto-discovery is not configured"})
		return
	}

	source, ok := h.resolveOwnedSource(c)
	if !ok {
		return
	}

	candidates, err := h.discoverer.Discover(ctx, source.Name)
	if err != nil {
		if errors.Is(err, repolink.ErrNotConfigured) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "repo-link auto-discovery is not configured"})
			return
		}
		slog.Error("Repo-link discovery failed", "error", err, "source_id", source.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "repo-link discovery failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"source_id":  source.ID,
		"candidates": candidates,
	})
}

// resolveOwnedSource validates the :id path parameter, loads the source, and
// verifies it belongs to the caller's organization. On any failure it writes the
// appropriate error response (400 invalid id / missing org, 404 not found or
// cross-org) and returns false. Cross-org access is reported as 404 to match the
// sources CRUD handlers (which do not distinguish missing from forbidden).
func (h *RepoLinkHandlers) resolveOwnedSource(c *gin.Context) (*models.StateSource, bool) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return nil, false
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source ID"})
		return nil, false
	}

	source, err := h.loadSource(c.Request.Context(), id)
	if err != nil {
		slog.Error("Failed to get state source for repo link", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state source"})
		return nil, false
	}
	if source == nil || source.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state source not found"})
		return nil, false
	}
	return source, true
}

// loadSource fetches a source by id; split out so resolveOwnedSource stays small.
func (h *RepoLinkHandlers) loadSource(ctx context.Context, id string) (*models.StateSource, error) {
	return h.sourceRepo.GetByID(ctx, id)
}
