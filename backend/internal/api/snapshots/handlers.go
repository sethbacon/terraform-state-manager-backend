// Package snapshots implements the HTTP handlers for state snapshot management:
// listing, retrieving, capturing, and comparing snapshots for drift analysis.
package snapshots

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	snapshotSvc "github.com/terraform-state-manager/terraform-state-manager/internal/services/snapshot"
)

// Handlers provides the HTTP handlers for snapshot and drift API endpoints.
type Handlers struct {
	snapshotRepo *repositories.StateSnapshotRepository
	driftRepo    *repositories.DriftEventRepository
	resultRepo   *repositories.AnalysisResultRepository
	runRepo      *repositories.AnalysisRunRepository
	snapshotSvc  *snapshotSvc.Service
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	snapshotRepo *repositories.StateSnapshotRepository,
	driftRepo *repositories.DriftEventRepository,
	resultRepo *repositories.AnalysisResultRepository,
	runRepo *repositories.AnalysisRunRepository,
	snapshotService *snapshotSvc.Service,
) *Handlers {
	return &Handlers{
		snapshotRepo: snapshotRepo,
		driftRepo:    driftRepo,
		resultRepo:   resultRepo,
		runRepo:      runRepo,
		snapshotSvc:  snapshotService,
	}
}

// ---------------------------------------------------------------------------
// Handler methods
// ---------------------------------------------------------------------------

// ListSnapshots handles GET /api/v1/snapshots.
// Returns a paginated list of snapshots for the organization, optionally
// filtered by workspace_name query parameter.
//
// @Summary      List snapshots
// @Description  Returns a paginated list of snapshots for the organization.
// @Tags         Snapshots
// @Produce      json
// @Param        limit           query  int     false  "Maximum number of snapshots to return (1-100, default 20)"
// @Param        offset          query  int     false  "Number of snapshots to skip (default 0)"
// @Param        workspace_name  query  string  false  "Filter by workspace name"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /snapshots [get]
func (h *Handlers) ListSnapshots(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	ctx := c.Request.Context()
	workspaceName := c.Query("workspace_name")

	var (
		snapshots []models.StateSnapshot
		total     int
		err       error
	)

	if workspaceName != "" {
		snapshots, total, err = h.snapshotRepo.ListByWorkspace(ctx, orgIDStr, workspaceName, limit, offset)
	} else {
		snapshots, total, err = h.snapshotRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	}

	if err != nil {
		slog.Error("Failed to list snapshots", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list snapshots"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   snapshots,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetSnapshot handles GET /api/v1/snapshots/:id.
// Returns a single snapshot by ID.
//
// @Summary      Get snapshot
// @Description  Returns a single snapshot by ID.
// @Tags         Snapshots
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /snapshots/{id} [get]
func (h *Handlers) GetSnapshot(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid snapshot ID"})
		return
	}

	ctx := c.Request.Context()
	snapshot, err := h.snapshotRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get snapshot", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve snapshot"})
		return
	}
	if snapshot == nil || snapshot.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "snapshot not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": snapshot})
}

// CaptureNow handles POST /api/v1/snapshots/capture.
// Triggers an immediate snapshot capture using results from the most recent
// completed analysis run for the organization.
//
// @Summary      Capture snapshot
// @Description  Triggers an immediate snapshot capture using results from the most recent completed analysis run.
// @Tags         Snapshots
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /snapshots/capture [post]
func (h *Handlers) CaptureNow(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()

	// Find the most recent completed analysis run for this organization.
	runs, _, err := h.runRepo.ListByOrganization(ctx, orgIDStr, 1, 0)
	if err != nil {
		slog.Error("Failed to find latest analysis run", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to find latest analysis run"})
		return
	}

	var latestRun *models.AnalysisRun
	for i := range runs {
		if runs[i].Status == models.RunStatusCompleted {
			latestRun = &runs[i]
			break
		}
	}

	if latestRun == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "no completed analysis run found",
			"message": "run an analysis first before capturing snapshots",
		})
		return
	}

	// Load all results for the latest run.
	results, _, err := h.resultRepo.ListByRunID(ctx, latestRun.ID, 1000, 0)
	if err != nil {
		slog.Error("Failed to load analysis results for snapshot capture", "error", err, "run_id", latestRun.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load analysis results"})
		return
	}

	if len(results) == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "no analysis results found for the latest run",
			"message": "the analysis run has no results to capture",
		})
		return
	}

	// Capture snapshots from the results.
	snapshots, err := h.snapshotSvc.CaptureFromAnalysisResults(ctx, orgIDStr, latestRun.SourceID, results)
	if err != nil {
		slog.Error("Failed to capture snapshots", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to capture snapshots"})
		return
	}

	slog.Info("Snapshots captured on demand",
		"org_id", orgIDStr, "count", len(snapshots), "run_id", latestRun.ID)

	c.JSON(http.StatusCreated, gin.H{
		"data":    snapshots,
		"message": "snapshots captured successfully",
		"count":   len(snapshots),
		"run_id":  latestRun.ID,
	})
}

// CompareSnapshots handles GET /api/v1/snapshots/compare?before=UUID&after=UUID.
// Compares two snapshots and returns the drift analysis.
//
// @Summary      Compare snapshots
// @Description  Compares two snapshots and returns the drift analysis.
// @Tags         Snapshots
// @Produce      json
// @Param        before  query  string  true  "ID of the before snapshot"
// @Param        after   query  string  true  "ID of the after snapshot"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /snapshots/compare [get]
func (h *Handlers) CompareSnapshots(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	beforeID := c.Query("before")
	afterID := c.Query("after")

	if beforeID == "" || afterID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "both 'before' and 'after' query parameters are required"})
		return
	}

	if _, err := uuid.Parse(beforeID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid 'before' snapshot ID"})
		return
	}
	if _, err := uuid.Parse(afterID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid 'after' snapshot ID"})
		return
	}

	ctx := c.Request.Context()

	// Verify both snapshots belong to the organization.
	beforeSnapshot, err := h.snapshotRepo.GetByID(ctx, beforeID)
	if err != nil {
		slog.Error("Failed to get before snapshot", "error", err, "id", beforeID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve before snapshot"})
		return
	}
	if beforeSnapshot == nil || beforeSnapshot.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "before snapshot not found"})
		return
	}

	afterSnapshot, err := h.snapshotRepo.GetByID(ctx, afterID)
	if err != nil {
		slog.Error("Failed to get after snapshot", "error", err, "id", afterID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve after snapshot"})
		return
	}
	if afterSnapshot == nil || afterSnapshot.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "after snapshot not found"})
		return
	}

	// Perform drift detection.
	changes, err := h.snapshotSvc.CompareSnapshots(ctx, beforeID, afterID)
	if err != nil {
		slog.Error("Failed to compare snapshots", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to compare snapshots"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"before":  beforeSnapshot,
			"after":   afterSnapshot,
			"changes": changes,
		},
	})
}

// ListDriftEvents handles GET /api/v1/snapshots/drift.
// Returns a paginated list of drift events for the organization, optionally
// filtered by workspace_name query parameter.
//
// @Summary      List drift events
// @Description  Returns a paginated list of drift events for the organization.
// @Tags         Drift
// @Produce      json
// @Param        limit           query  int     false  "Maximum number of events to return (1-100, default 20)"
// @Param        offset          query  int     false  "Number of events to skip (default 0)"
// @Param        workspace_name  query  string  false  "Filter by workspace name"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /drift/events [get]
func (h *Handlers) ListDriftEvents(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	ctx := c.Request.Context()
	workspaceName := c.Query("workspace_name")

	var (
		events []models.DriftEvent
		total  int
		err    error
	)

	if workspaceName != "" {
		events, total, err = h.driftRepo.ListByWorkspace(ctx, orgIDStr, workspaceName, limit, offset)
	} else {
		events, total, err = h.driftRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	}

	if err != nil {
		slog.Error("Failed to list drift events", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list drift events"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   events,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetDriftEvent handles GET /api/v1/snapshots/drift/:id.
// Returns a single drift event by ID.
//
// @Summary      Get drift event
// @Description  Returns a single drift event by ID.
// @Tags         Drift
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /drift/events/{id} [get]
func (h *Handlers) GetDriftEvent(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid drift event ID"})
		return
	}

	ctx := c.Request.Context()
	event, err := h.driftRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get drift event", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve drift event"})
		return
	}
	if event == nil || event.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "drift event not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": event})
}
