// Package analysis implements the HTTP handlers for managing state analysis
// runs: starting, listing, cancelling, and retrieving results.
package analysis

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/validation"
)

// Handlers provides the HTTP handlers for analysis run API endpoints.
type Handlers struct {
	cfg         *config.Config
	runRepo     *repositories.AnalysisRunRepository
	resultRepo  *repositories.AnalysisResultRepository
	sourceRepo  *repositories.StateSourceRepository
	tokenCipher *crypto.TokenCipher

	// cancelFns tracks in-flight analysis run contexts so they can be
	// cancelled from the CancelRun endpoint.
	cancelFns sync.Map // map[string]context.CancelFunc
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	cfg *config.Config,
	runRepo *repositories.AnalysisRunRepository,
	resultRepo *repositories.AnalysisResultRepository,
	sourceRepo *repositories.StateSourceRepository,
	tokenCipher *crypto.TokenCipher,
) *Handlers {
	return &Handlers{
		cfg:         cfg,
		runRepo:     runRepo,
		resultRepo:  resultRepo,
		sourceRepo:  sourceRepo,
		tokenCipher: tokenCipher,
	}
}

// ---------------------------------------------------------------------------
// Handler methods
// ---------------------------------------------------------------------------

// StartRun handles POST /api/v1/analysis/run.
// Creates a new analysis run record, then starts the analysis in a background
// goroutine. Returns the run ID immediately (async operation).
// @Summary      Start analysis run
// @Description  Creates a new analysis run and starts it asynchronously; returns the run ID immediately
// @Tags         Analysis
// @Accept       json
// @Produce      json
// @Param        request  body      models.AnalysisRunCreateRequest  true  "Analysis run creation request"
// @Success      202  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /analysis/run [post]
func (h *Handlers) StartRun(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)

	var req models.AnalysisRunCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	// Validate source_id if provided.
	if req.SourceID != nil && *req.SourceID != "" {
		if _, err := uuid.Parse(*req.SourceID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_id"})
			return
		}

		ctx := c.Request.Context()
		source, err := h.sourceRepo.GetByID(ctx, *req.SourceID)
		if err != nil {
			slog.Error("Failed to validate source for analysis run",
				"error", err, "source_id", *req.SourceID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate source"})
			return
		}
		if source == nil || source.OrganizationID != orgIDStr {
			c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
			return
		}
		if !source.IsActive {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source is not active"})
			return
		}
	}

	// Create the run record with status=pending.
	runConfig, _ := json.Marshal(map[string]interface{}{})
	if req.Config != nil {
		runConfig = *req.Config
	}

	run := &models.AnalysisRun{
		OrganizationID: orgIDStr,
		SourceID:       req.SourceID,
		Status:         models.RunStatusPending,
		TriggerType:    req.TriggerType,
		Config:         runConfig,
		TriggeredBy:    nilIfEmpty(userIDStr),
	}

	ctx := c.Request.Context()
	if err := h.runRepo.Create(ctx, run); err != nil {
		slog.Error("Failed to create analysis run", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create analysis run"})
		return
	}

	// Launch the analysis goroutine with a cancellable context.
	runCtx, cancel := context.WithCancel(context.Background())
	h.cancelFns.Store(run.ID, cancel)

	go h.executeAnalysis(runCtx, run.ID, orgIDStr)

	slog.Info("Analysis run started",
		"run_id", run.ID, "source_id", run.SourceID, "trigger", run.TriggerType)

	c.JSON(http.StatusAccepted, gin.H{
		"data": gin.H{
			"id":     run.ID,
			"status": run.Status,
		},
		"message": "analysis run started",
	})
}

// ListRuns handles GET /api/v1/analysis/runs.
// Returns a paginated list of analysis runs for the organization.
// @Summary      List analysis runs
// @Description  Returns a paginated list of analysis runs for the organization
// @Tags         Analysis
// @Produce      json
// @Param        source_id  query     string  false  "Filter by source ID"
// @Param        status     query     string  false  "Filter by run status"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /analysis/runs [get]
func (h *Handlers) ListRuns(c *gin.Context) {
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
	runs, total, err := h.runRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list analysis runs", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list analysis runs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   runs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetRun handles GET /api/v1/analysis/runs/:id.
// Returns the run details including summary counts.
// @Summary      Get analysis run
// @Description  Returns the run details including summary counts
// @Tags         Analysis
// @Produce      json
// @Param        id  path      string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /analysis/runs/{id} [get]
func (h *Handlers) GetRun(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run ID"})
		return
	}

	ctx := c.Request.Context()
	run, err := h.runRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get analysis run", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve analysis run"})
		return
	}
	if run == nil || run.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "analysis run not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": run})
}

// GetRunResults handles GET /api/v1/analysis/runs/:id/results.
// Returns paginated workspace results for a specific run.
// @Summary      Get analysis run results
// @Description  Returns paginated workspace results for a specific analysis run
// @Tags         Analysis
// @Produce      json
// @Param        id  path      string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /analysis/runs/{id}/results [get]
func (h *Handlers) GetRunResults(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run ID"})
		return
	}

	ctx := c.Request.Context()

	// Verify the run belongs to the caller's organization.
	run, err := h.runRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get analysis run for results", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve analysis run"})
		return
	}
	if run == nil || run.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "analysis run not found"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 1000 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	results, total, err := h.resultRepo.ListByRunID(ctx, id, limit, offset)
	if err != nil {
		slog.Error("Failed to list analysis results", "error", err, "run_id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list analysis results"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   results,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// CancelRun handles POST /api/v1/analysis/runs/:id/cancel.
// Sets the run status to cancelled and cancels its context.
// @Summary      Cancel analysis run
// @Description  Sets the run status to cancelled and cancels its in-flight context
// @Tags         Analysis
// @Produce      json
// @Param        id  path      string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /analysis/runs/{id}/cancel [post]
func (h *Handlers) CancelRun(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run ID"})
		return
	}

	ctx := c.Request.Context()
	run, err := h.runRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get analysis run for cancellation", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve analysis run"})
		return
	}
	if run == nil || run.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "analysis run not found"})
		return
	}

	// Only pending or running runs can be cancelled.
	if run.Status != models.RunStatusPending && run.Status != models.RunStatusRunning {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "only pending or running analysis runs can be cancelled",
		})
		return
	}

	// Cancel the in-flight context if it exists.
	if cancelFn, loaded := h.cancelFns.LoadAndDelete(id); loaded {
		if fn, ok := cancelFn.(context.CancelFunc); ok {
			fn()
		}
	}

	if err := h.runRepo.UpdateStatus(ctx, id, models.RunStatusCancelled); err != nil {
		slog.Error("Failed to cancel analysis run", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel analysis run"})
		return
	}

	slog.Info("Analysis run cancelled", "run_id", id)
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":     id,
			"status": models.RunStatusCancelled,
		},
		"message": "analysis run cancelled",
	})
}

// DeleteRun handles DELETE /api/v1/analysis/runs/:id.
// Removes a completed, failed, or cancelled analysis run and its results.
// @Summary      Delete analysis run
// @Description  Removes a completed, failed, or cancelled analysis run and its results
// @Tags         Analysis
// @Produce      json
// @Param        id  path      string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /analysis/runs/{id} [delete]
func (h *Handlers) DeleteRun(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run ID"})
		return
	}

	ctx := c.Request.Context()
	run, err := h.runRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get analysis run for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve analysis run"})
		return
	}
	if run == nil || run.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "analysis run not found"})
		return
	}

	if run.Status == models.RunStatusPending || run.Status == models.RunStatusRunning {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot delete a running or pending analysis run; cancel it first"})
		return
	}

	if err := h.runRepo.Delete(ctx, id); err != nil {
		slog.Error("Failed to delete analysis run", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete analysis run"})
		return
	}

	slog.Info("Analysis run deleted", "run_id", id)
	c.JSON(http.StatusOK, gin.H{"message": "analysis run deleted"})
}

// GetLatestSummary handles GET /api/v1/analysis/summary.
// Returns aggregated summary from the latest completed run.
// @Summary      Get analysis summary
// @Description  Returns aggregated summary from the latest completed analysis run
// @Tags         Analysis
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /analysis/summary [get]
func (h *Handlers) GetLatestSummary(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()
	summary, err := h.resultRepo.GetLatestSummary(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to get latest analysis summary", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve analysis summary"})
		return
	}
	if summary == nil {
		c.JSON(http.StatusOK, gin.H{
			"data":    nil,
			"message": "no completed analysis runs found",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": summary})
}

// ---------------------------------------------------------------------------
// Background analysis execution
// ---------------------------------------------------------------------------

// executeAnalysis is the goroutine entry point for running a state analysis.
// It updates the run record through its lifecycle: pending -> running -> completed/failed.
func (h *Handlers) executeAnalysis(ctx context.Context, runID, orgID string) {
	defer h.cancelFns.Delete(runID)

	startTime := time.Now()
	logger := slog.With("run_id", runID, "org_id", orgID)

	// Use a background context for DB operations so they complete even if
	// the analysis context is cancelled.
	dbCtx := context.Background()

	// a. Update status to running.
	now := time.Now()
	run, err := h.runRepo.GetByID(dbCtx, runID)
	if err != nil || run == nil {
		logger.Error("Failed to load analysis run", "error", err)
		return
	}
	run.Status = models.RunStatusRunning
	run.StartedAt = &now
	if err := h.runRepo.Update(dbCtx, run); err != nil {
		logger.Error("Failed to update run to running", "error", err)
		return
	}

	// b. Load and decrypt source configuration.
	if run.SourceID == nil || *run.SourceID == "" {
		h.failRun(dbCtx, run, "no source_id configured for this run", startTime)
		return
	}

	source, err := h.sourceRepo.GetByID(dbCtx, *run.SourceID)
	if err != nil || source == nil {
		h.failRun(dbCtx, run, "failed to load source: "+errString(err), startTime)
		return
	}

	decryptedConfig, err := h.decryptConfig(source.Config)
	if err != nil {
		h.failRun(dbCtx, run, "failed to decrypt source config: "+err.Error(), startTime)
		return
	}

	// c. Create the appropriate client.
	client, err := clients.NewClientFromConfig(source.SourceType, decryptedConfig)
	if err != nil {
		h.failRun(dbCtx, run, "failed to create client: "+err.Error(), startTime)
		return
	}

	// d. Run the analysis based on client type.
	var results []models.AnalysisResult

	switch c := client.(type) {
	case *clients.HCPClientAdapter:
		results = h.analyzeHCP(ctx, c, runID, source)
	default:
		// For other backends, test connection only (full analysis in future phases).
		if testErr := client.TestConnection(ctx); testErr != nil {
			h.failRun(dbCtx, run, "connection test failed: "+testErr.Error(), startTime)
			return
		}
		logger.Info("Connection test passed for non-HCP source; full analysis not yet implemented",
			"source_type", source.SourceType)
	}

	// Check for cancellation.
	if ctx.Err() != nil {
		logger.Info("Analysis run was cancelled")
		return
	}

	// e. Save results to DB.
	if len(results) > 0 {
		if err := h.resultRepo.BulkCreate(dbCtx, results); err != nil {
			logger.Error("Failed to save analysis results", "error", err)
			h.failRun(dbCtx, run, "failed to save results: "+err.Error(), startTime)
			return
		}
	}

	// f. Calculate summary counts and update run record.
	var (
		totalWorkspaces  = len(results)
		successCount     int
		failCount        int
		totalRUM         int
		totalManaged     int
		totalResources   int
		totalDataSources int
	)
	for _, r := range results {
		switch r.Status {
		case models.ResultStatusSuccess:
			successCount++
		case models.ResultStatusFailed:
			failCount++
		}
		totalRUM += r.RUMCount
		totalManaged += r.ManagedCount
		totalResources += r.TotalResources
		totalDataSources += r.DataSourceCount
	}

	elapsed := int(time.Since(startTime).Milliseconds())
	completedAt := time.Now()

	run.Status = models.RunStatusCompleted
	run.CompletedAt = &completedAt
	run.TotalWorkspaces = totalWorkspaces
	run.SuccessfulCount = successCount
	run.FailedCount = failCount
	run.TotalRUM = totalRUM
	run.TotalManaged = totalManaged
	run.TotalResources = totalResources
	run.TotalDataSources = totalDataSources
	run.PerformanceMS = &elapsed

	if err := h.runRepo.Update(dbCtx, run); err != nil {
		logger.Error("Failed to update run with final status", "error", err)
		return
	}

	logger.Info("Analysis run completed",
		"workspaces", totalWorkspaces,
		"success", successCount,
		"failed", failCount,
		"resources", totalResources,
		"duration_ms", elapsed)
}

// analyzeHCP performs analysis against an HCP Terraform source by listing all
// workspaces and downloading/inspecting each workspace's current state file.
func (h *Handlers) analyzeHCP(
	ctx context.Context,
	client *clients.HCPClientAdapter,
	runID string,
	source *models.StateSource,
) []models.AnalysisResult {
	logger := slog.With("run_id", runID, "source_type", "hcp_terraform")

	workspaces, err := client.GetAllWorkspaces(ctx, client.Organization)
	if err != nil {
		logger.Error("Failed to list HCP workspaces", "error", err)
		return nil
	}

	logger.Info("Fetched workspaces for analysis", "count", len(workspaces))

	results := make([]models.AnalysisResult, 0, len(workspaces))

	for _, ws := range workspaces {
		// Check cancellation between workspaces.
		if ctx.Err() != nil {
			logger.Info("Analysis cancelled, stopping workspace iteration")
			break
		}

		result := h.analyzeHCPWorkspace(ctx, client, runID, ws)
		results = append(results, result)
	}

	return results
}

// analyzeHCPWorkspace downloads and analyzes the state for a single HCP workspace.
func (h *Handlers) analyzeHCPWorkspace(
	ctx context.Context,
	client *clients.HCPClientAdapter,
	runID string,
	ws hcp.Workspace,
) models.AnalysisResult {
	emptyJSON := json.RawMessage(`{}`)
	result := models.AnalysisResult{
		RunID:             runID,
		WorkspaceName:     ws.Name,
		WorkspaceID:       &ws.ID,
		Organization:      &ws.Organization,
		Status:            models.ResultStatusSuccess,
		ResourcesByType:   emptyJSON,
		ResourcesByModule: emptyJSON,
		ProviderAnalysis:  emptyJSON,
	}

	// If the workspace has no state download URL, skip it.
	if ws.StateDownloadURL == "" {
		result.Status = models.ResultStatusSkipped
		errType := models.ErrorTypeStateNotFound
		errMsg := "no state file available for this workspace"
		result.ErrorType = &errType
		result.ErrorMessage = &errMsg
		return result
	}

	// Download the state file.
	stateFile, err := client.DownloadState(ctx, ws.StateDownloadURL)
	if err != nil {
		result.Status = models.ResultStatusFailed
		errType := categorizeError(err)
		errMsg := err.Error()
		result.ErrorType = &errType
		result.ErrorMessage = &errMsg
		return result
	}

	if stateFile == nil {
		result.Status = models.ResultStatusSkipped
		errType := models.ErrorTypeStateNotFound
		errMsg := "state file download returned nil"
		result.ErrorType = &errType
		result.ErrorMessage = &errMsg
		return result
	}

	// Validate the state file.
	vr := validation.ValidateStateFile(stateFile)
	if !vr.IsValid {
		result.Status = models.ResultStatusFailed
		errType := models.ErrorTypeException
		errMsg := strings.Join(vr.Errors, "; ")
		result.ErrorType = &errType
		result.ErrorMessage = &errMsg
		return result
	}

	// Analyze resource counts.
	analyzeResources(stateFile, &result)

	// Set state metadata.
	if stateFile.TerraformVersion != "" {
		result.TerraformVersion = &stateFile.TerraformVersion
	}
	serial := stateFile.Serial
	result.StateSerial = &serial
	if stateFile.Lineage != "" {
		result.StateLineage = &stateFile.Lineage
	}

	method := "api"
	result.AnalysisMethod = &method

	return result
}

// analyzeResources processes a state file and populates the resource counts
// on the analysis result.
func analyzeResources(state *hcp.StateFile, result *models.AnalysisResult) {
	resourcesByType := make(map[string]int)
	resourcesByModule := make(map[string]int)
	providerCounts := make(map[string]int)

	var (
		totalResources    int
		managedCount      int
		dataSourceCount   int
		nullResourceCount int
	)

	for _, res := range state.Resources {
		instanceCount := len(res.Instances)
		totalResources += instanceCount
		resourcesByType[res.Type] += instanceCount

		module := res.Module
		if module == "" {
			module = "root"
		}
		resourcesByModule[module] += instanceCount

		// Extract provider short name from the provider string.
		// Formats seen in state files:
		//   "provider[\"registry.terraform.io/hashicorp/aws\"]"
		//   "provider[\"registry.terraform.io/hashicorp/aws\"].euw1"  (aliased)
		//   "registry.terraform.io/hashicorp/aws"
		providerName := res.Provider

		// Strip the provider["..."] wrapper, ignoring any alias suffix after "]"
		if idx := strings.Index(providerName, "[\""); idx >= 0 {
			providerName = providerName[idx+2:]
			if endIdx := strings.Index(providerName, "\"]"); endIdx >= 0 {
				providerName = providerName[:endIdx]
			}
		}

		// Take the last segment of the registry path: "registry.terraform.io/hashicorp/aws" -> "aws"
		if parts := strings.Split(providerName, "/"); len(parts) > 0 {
			providerName = parts[len(parts)-1]
		}
		providerCounts[providerName] += instanceCount

		switch res.Mode {
		case "managed":
			managedCount += instanceCount
			if res.Type == "null_resource" || res.Type == "terraform_data" {
				nullResourceCount += instanceCount
			}
		case "data":
			dataSourceCount += instanceCount
		}
	}

	result.TotalResources = totalResources
	result.ManagedCount = managedCount
	result.DataSourceCount = dataSourceCount
	result.NullResourceCount = nullResourceCount
	// RUM = managed resources minus null_resource and terraform_data
	result.RUMCount = managedCount - nullResourceCount

	// Marshal breakdown maps into JSON.
	if data, err := json.Marshal(resourcesByType); err == nil {
		result.ResourcesByType = data
	}
	if data, err := json.Marshal(resourcesByModule); err == nil {
		result.ResourcesByModule = data
	}
	if data, err := json.Marshal(providerCounts); err == nil {
		result.ProviderAnalysis = data
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// decryptConfig decrypts sensitive values within a JSON config map.
func (h *Handlers) decryptConfig(rawConfig json.RawMessage) (json.RawMessage, error) {
	var configMap map[string]interface{}
	if err := json.Unmarshal(rawConfig, &configMap); err != nil {
		return rawConfig, err
	}

	for k, v := range configMap {
		if !validation.IsSensitiveConfigKey(k) {
			continue
		}
		str, ok := v.(string)
		if !ok || str == "" {
			continue
		}
		decrypted, err := h.tokenCipher.Open(str)
		if err != nil {
			slog.Warn("Failed to decrypt config field", "key", k, "error", err)
			continue
		}
		configMap[k] = decrypted
	}

	return json.Marshal(configMap)
}

// failRun marks a run as failed with the given error message and updates the
// run record in the database.
func (h *Handlers) failRun(ctx context.Context, run *models.AnalysisRun, errMsg string, startTime time.Time) {
	now := time.Now()
	elapsed := int(now.Sub(startTime).Milliseconds())

	run.Status = models.RunStatusFailed
	run.CompletedAt = &now
	run.ErrorMessage = &errMsg
	run.PerformanceMS = &elapsed

	if err := h.runRepo.Update(ctx, run); err != nil {
		slog.Error("Failed to mark run as failed",
			"run_id", run.ID, "error", err, "original_error", errMsg)
	} else {
		slog.Warn("Analysis run failed",
			"run_id", run.ID, "reason", errMsg, "duration_ms", elapsed)
	}
}

// categorizeError maps common error conditions to ErrorType constants.
func categorizeError(err error) string {
	if err == nil {
		return models.ErrorTypeUnknown
	}
	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "unauthorized") || strings.Contains(msg, "401"):
		return models.ErrorTypeUnauthorized
	case strings.Contains(msg, "forbidden") || strings.Contains(msg, "403"):
		return models.ErrorTypePermissionDenied
	case strings.Contains(msg, "not found") || strings.Contains(msg, "404"):
		return models.ErrorTypeStateNotFound
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return models.ErrorTypeTimeout
	default:
		return models.ErrorTypeException
	}
}

// errString returns the error message or "unknown error" for nil.
func errString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

// nilIfEmpty returns a pointer to s if non-empty, otherwise nil.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
