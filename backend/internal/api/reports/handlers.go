// Package reports implements the HTTP handlers for report generation,
// listing, downloading, and deletion.
package reports

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/reporter"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage"
)

// Handlers provides the HTTP handlers for report API endpoints.
type Handlers struct {
	cfg        *config.Config
	reportRepo *repositories.ReportRepository
	runRepo    *repositories.AnalysisRunRepository
	resultRepo *repositories.AnalysisResultRepository
	storage    storage.Backend
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	cfg *config.Config,
	reportRepo *repositories.ReportRepository,
	runRepo *repositories.AnalysisRunRepository,
	resultRepo *repositories.AnalysisResultRepository,
	storageBackend storage.Backend,
) *Handlers {
	return &Handlers{
		cfg:        cfg,
		reportRepo: reportRepo,
		runRepo:    runRepo,
		resultRepo: resultRepo,
		storage:    storageBackend,
	}
}

// ---------------------------------------------------------------------------
// Handler methods
// ---------------------------------------------------------------------------

// GenerateReport handles POST /api/v1/reports/generate.
// Takes a run_id and format, generates the report, stores it via the storage
// backend, and returns the report metadata.
// @Summary      Generate report
// @Description  Generates a report for a completed analysis run and stores it via the storage backend
// @Tags         Reports
// @Accept       json
// @Produce      json
// @Param        request  body      models.ReportCreateRequest  true  "Report creation request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /reports/generate [post]
func (h *Handlers) GenerateReport(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)

	var req models.ReportCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	// Validate format.
	if !reporter.IsValidFormat(req.Format) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":             "unsupported report format",
			"supported_formats": reporter.SupportedFormats(),
		})
		return
	}

	// Validate run_id.
	if _, err := uuid.Parse(req.RunID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run_id"})
		return
	}

	ctx := c.Request.Context()

	// Verify the run exists and belongs to the organization.
	run, err := h.runRepo.GetByID(ctx, req.RunID)
	if err != nil {
		slog.Error("Failed to get analysis run for report generation", "error", err, "run_id", req.RunID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve analysis run"})
		return
	}
	if run == nil || run.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "analysis run not found"})
		return
	}
	if run.Status != models.RunStatusCompleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "can only generate reports for completed analysis runs"})
		return
	}

	// Fetch all results for this run (unpaginated; we need them all for the report).
	results, _, err := h.resultRepo.ListByRunID(ctx, req.RunID, 10000, 0)
	if err != nil {
		slog.Error("Failed to list analysis results for report", "error", err, "run_id", req.RunID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve analysis results"})
		return
	}

	// Reconstruct AnalysisOutput from DB records.
	analysisOutput := buildAnalysisOutput(run, results)

	// Generate the report.
	data, filename, err := reporter.GenerateReport(analysisOutput, req.Format)
	if err != nil {
		slog.Error("Failed to generate report", "error", err, "format", req.Format)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate report"})
		return
	}

	// Store the report file.
	storagePath := fmt.Sprintf("reports/%s/%s/%s", orgIDStr, req.RunID, filename)
	if err := h.storage.Put(ctx, storagePath, data); err != nil {
		slog.Error("Failed to store report", "error", err, "path", storagePath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store report"})
		return
	}

	// Save report metadata in DB.
	fileSize := int64(len(data))
	reportName := filename
	if req.Name != "" {
		extMap := map[string]string{
			"markdown": ".md",
			"json":     ".json",
			"csv":      ".csv",
		}
		ext := extMap[req.Format]
		if ext != "" && !strings.HasSuffix(req.Name, ext) {
			reportName = req.Name + ext
		} else {
			reportName = req.Name
		}
	}
	report := &models.Report{
		OrganizationID: orgIDStr,
		RunID:          &req.RunID,
		Name:           reportName,
		Format:         req.Format,
		StoragePath:    storagePath,
		FileSizeBytes:  &fileSize,
		GeneratedBy:    nilIfEmpty(userIDStr),
	}

	if err := h.reportRepo.Create(ctx, report); err != nil {
		slog.Error("Failed to save report metadata", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save report metadata"})
		return
	}

	slog.Info("Report generated",
		"report_id", report.ID, "format", req.Format, "run_id", req.RunID, "size", fileSize)

	c.JSON(http.StatusCreated, gin.H{"data": report})
}

// ListReports handles GET /api/v1/reports.
// Returns a paginated list of reports for the organization.
// @Summary      List reports
// @Description  Returns a paginated list of reports for the organization
// @Tags         Reports
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /reports [get]
func (h *Handlers) ListReports(c *gin.Context) {
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
	reports, total, err := h.reportRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list reports", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list reports"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   reports,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetReport handles GET /api/v1/reports/:id.
// Returns report metadata.
// @Summary      Get report
// @Description  Returns report metadata
// @Tags         Reports
// @Produce      json
// @Param        id  path      string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /reports/{id} [get]
func (h *Handlers) GetReport(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid report ID"})
		return
	}

	ctx := c.Request.Context()
	report, err := h.reportRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get report", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve report"})
		return
	}
	if report == nil || report.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "report not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": report})
}

// DownloadReport handles GET /api/v1/reports/:id/download.
// Streams the report file to the client.
// @Summary      Download report file
// @Description  Streams the report file to the client
// @Tags         Reports
// @Produce      application/octet-stream
// @Param        id  path      string  true  "Resource ID"
// @Success      200  {file}    binary
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /reports/{id}/download [get]
func (h *Handlers) DownloadReport(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid report ID"})
		return
	}

	ctx := c.Request.Context()
	report, err := h.reportRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get report for download", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve report"})
		return
	}
	if report == nil || report.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "report not found"})
		return
	}

	// Retrieve the file from storage.
	data, err := h.storage.Get(ctx, report.StoragePath)
	if err != nil {
		slog.Error("Failed to retrieve report file from storage", "error", err, "path", report.StoragePath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve report file"})
		return
	}

	// Set content type based on format.
	contentType := "application/octet-stream"
	switch report.Format {
	case "markdown":
		contentType = "text/markdown; charset=utf-8"
	case "json":
		contentType = "application/json; charset=utf-8"
	case "csv":
		contentType = "text/csv; charset=utf-8"
	}

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", report.Name))
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", fmt.Sprintf("%d", len(data)))
	c.Data(http.StatusOK, contentType, data)
}

// DeleteReport handles DELETE /api/v1/reports/:id.
// Deletes the report metadata and the stored file.
// @Summary      Delete report
// @Description  Deletes the report metadata and the stored file
// @Tags         Reports
// @Produce      json
// @Param        id  path      string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /reports/{id} [delete]
func (h *Handlers) DeleteReport(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid report ID"})
		return
	}

	ctx := c.Request.Context()
	report, err := h.reportRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get report for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve report"})
		return
	}
	if report == nil || report.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "report not found"})
		return
	}

	// Delete from storage (best effort; continue even if storage delete fails).
	if err := h.storage.Delete(ctx, report.StoragePath); err != nil {
		slog.Warn("Failed to delete report file from storage",
			"error", err, "path", report.StoragePath, "report_id", id)
	}

	// Delete from database.
	if err := h.reportRepo.Delete(ctx, id); err != nil {
		slog.Error("Failed to delete report", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete report"})
		return
	}

	slog.Info("Report deleted", "report_id", id, "name", report.Name)
	c.JSON(http.StatusOK, gin.H{
		"message": "report deleted successfully",
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildAnalysisOutput reconstructs an analyzer.AnalysisOutput from DB records
// so that the reporter package can generate reports.
func buildAnalysisOutput(run *models.AnalysisRun, results []models.AnalysisResult) *analyzer.AnalysisOutput {
	workspaceResults := make([]analyzer.WorkspaceResult, 0, len(results))

	// Aggregation maps for the summary.
	organizations := make(map[string]int)
	topResourceTypes := make(map[string]int)

	allProviders := make(map[string]*analyzer.ProviderUsageSummary)
	providerResourceTypes := make(map[string]map[string]bool)
	terraformVersions := make(map[string]int)

	for _, r := range results {
		ws := hcp.Workspace{
			ID:           derefString(r.WorkspaceID),
			Name:         r.WorkspaceName,
			Organization: derefString(r.Organization),
		}

		var wsResult analyzer.WorkspaceResult
		wsResult.Workspace = ws
		wsResult.Method = derefString(r.AnalysisMethod)

		if r.Status == models.ResultStatusFailed || r.Status == models.ResultStatusSkipped {
			errType := derefString(r.ErrorType)
			errMsg := derefString(r.ErrorMessage)
			wsResult.Error = &analyzer.AnalysisError{
				ErrorType: errType,
				Message:   errMsg,
			}
		} else {
			// Build ResourceCounts from DB fields.
			counts := analyzer.NewResourceCounts()
			counts.Total = r.TotalResources
			counts.Managed = r.ManagedCount
			counts.RUM = r.RUMCount
			counts.DataSources = r.DataSourceCount
			counts.ExcludedNull = r.NullResourceCount
			counts.TerraformVersion = derefString(r.TerraformVersion)
			counts.Serial = derefInt(r.StateSerial)
			counts.Lineage = derefString(r.StateLineage)

			// Unmarshal JSONB fields.
			if len(r.ResourcesByType) > 0 {
				var byType map[string]int
				if err := json.Unmarshal(r.ResourcesByType, &byType); err == nil {
					counts.ByType = byType
				}
			}
			if len(r.ResourcesByModule) > 0 {
				var byModule map[string]int
				if err := json.Unmarshal(r.ResourcesByModule, &byModule); err == nil {
					counts.ByModule = byModule
				}
			}
			if len(r.ProviderAnalysis) > 0 {
				var pa analyzer.ProviderAnalysis
				if err := json.Unmarshal(r.ProviderAnalysis, &pa); err == nil {
					counts.ProviderAnalysis = &pa
				}
			}

			wsResult.Counts = counts

			// Aggregate for summary.
			if ws.Organization != "" {
				organizations[ws.Organization]++
			}
			for resourceType, count := range counts.ByType {
				topResourceTypes[resourceType] += count
			}

			// Aggregate provider data.
			if counts.ProviderAnalysis != nil {
				for providerName, usage := range counts.ProviderAnalysis.ProviderUsage {
					if _, exists := allProviders[providerName]; !exists {
						allProviders[providerName] = &analyzer.ProviderUsageSummary{}
						providerResourceTypes[providerName] = make(map[string]bool)
					}
					allProviders[providerName].TotalResources += usage.ResourceCount
					allProviders[providerName].WorkspacesUsing++
					for _, rt := range usage.ResourceTypes {
						providerResourceTypes[providerName][rt] = true
					}
				}
				if counts.ProviderAnalysis.VersionAnalysis != nil && counts.ProviderAnalysis.VersionAnalysis.TerraformVersion != "" {
					terraformVersions[counts.ProviderAnalysis.VersionAnalysis.TerraformVersion]++
				}
			}
		}

		workspaceResults = append(workspaceResults, wsResult)
	}

	// Finalize provider resource types.
	for providerName, pSummary := range allProviders {
		if types, ok := providerResourceTypes[providerName]; ok {
			typeSlice := make([]string, 0, len(types))
			for t := range types {
				typeSlice = append(typeSlice, t)
			}
			sort.Strings(typeSlice)
			pSummary.ResourceTypes = typeSlice
		}
	}

	// Build summary.
	summary := &analyzer.AnalysisSummary{
		TotalRUM:         run.TotalRUM,
		TotalManaged:     run.TotalManaged,
		TotalResources:   run.TotalResources,
		TotalDataSources: run.TotalDataSources,
		TotalWorkspaces:  run.TotalWorkspaces,
		Organizations:    organizations,
		TopResourceTypes: topResourceTypes,
	}

	if len(allProviders) > 0 {
		summary.ProviderSummary = &analyzer.ProviderSummaryData{
			AllProviders:      allProviders,
			TerraformVersions: terraformVersions,
		}
	}

	perfMS := int64(0)
	if run.PerformanceMS != nil {
		perfMS = int64(*run.PerformanceMS)
	}

	return &analyzer.AnalysisOutput{
		Results:         workspaceResults,
		Summary:         summary,
		PerformanceMS:   perfMS,
		TotalWorkspaces: run.TotalWorkspaces,
		SuccessCount:    run.SuccessfulCount,
		FailCount:       run.FailedCount,
	}
}

// nilIfEmpty returns a pointer to s if non-empty, otherwise nil.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefString returns the value of a *string or empty string if nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefInt returns the value of a *int or 0 if nil.
func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}
