// Package dashboards implements the HTTP handlers for the dashboard API,
// providing aggregated views of analysis data for the frontend dashboard.
package dashboards

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Handlers provides the HTTP handlers for dashboard API endpoints.
type Handlers struct {
	db         *sql.DB
	runRepo    *repositories.AnalysisRunRepository
	resultRepo *repositories.AnalysisResultRepository
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	db *sql.DB,
	runRepo *repositories.AnalysisRunRepository,
	resultRepo *repositories.AnalysisResultRepository,
) *Handlers {
	return &Handlers{
		db:         db,
		runRepo:    runRepo,
		resultRepo: resultRepo,
	}
}

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

// OverviewResponse holds the dashboard overview data.
type OverviewResponse struct {
	TotalRUM            int    `json:"total_rum"`
	TotalManaged        int    `json:"total_managed"`
	TotalResources      int    `json:"total_resources"`
	TotalDataSources    int    `json:"total_data_sources"`
	TotalWorkspaces     int    `json:"total_workspaces"`
	SuccessfulWorkspace int    `json:"successful_workspaces"`
	FailedWorkspaces    int    `json:"failed_workspaces"`
	LastRunAt           string `json:"last_run_at,omitempty"`
	RunID               string `json:"run_id"`
	RunStatus           string `json:"run_status"`
}

// ResourceBreakdownEntry represents a single resource type and its count.
type ResourceBreakdownEntry struct {
	ResourceType string `json:"resource_type"`
	Count        int    `json:"count"`
}

// ProviderDistributionEntry represents provider usage data.
type ProviderDistributionEntry struct {
	Provider       string `json:"provider"`
	ResourceCount  int    `json:"resource_count"`
	WorkspaceCount int    `json:"workspace_count"`
}

// TrendPoint represents a single data point in a historical trend.
type TrendPoint struct {
	RunID           string `json:"run_id"`
	CompletedAt     string `json:"completed_at"`
	TotalRUM        int    `json:"total_rum"`
	TotalManaged    int    `json:"total_managed"`
	TotalResources  int    `json:"total_resources"`
	TotalWorkspaces int    `json:"total_workspaces"`
	SuccessfulCount int    `json:"successful_count"`
	FailedCount     int    `json:"failed_count"`
}

// VersionDistributionEntry represents a Terraform version and its workspace count.
type VersionDistributionEntry struct {
	Version string `json:"version"`
	Count   int    `json:"count"`
}

// ---------------------------------------------------------------------------
// Handler methods
// ---------------------------------------------------------------------------

// GetOverview handles GET /api/v1/dashboard/overview.
// Returns summary statistics from the latest completed analysis run.
func (h *Handlers) GetOverview(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()

	// Find the latest completed run for this organization.
	run, err := h.getLatestCompletedRun(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to get latest completed run for overview", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve dashboard overview"})
		return
	}
	if run == nil {
		c.JSON(http.StatusOK, gin.H{
			"data":    nil,
			"message": "no completed analysis runs found",
		})
		return
	}

	overview := &OverviewResponse{
		TotalRUM:            run.TotalRUM,
		TotalManaged:        run.TotalManaged,
		TotalResources:      run.TotalResources,
		TotalDataSources:    run.TotalDataSources,
		TotalWorkspaces:     run.TotalWorkspaces,
		SuccessfulWorkspace: run.SuccessfulCount,
		FailedWorkspaces:    run.FailedCount,
		RunID:               run.ID,
		RunStatus:           run.Status,
	}
	if run.CompletedAt != nil {
		overview.LastRunAt = run.CompletedAt.Format("2006-01-02T15:04:05Z")
	}

	c.JSON(http.StatusOK, gin.H{"data": overview})
}

// GetResourceBreakdown handles GET /api/v1/dashboard/resources.
// Returns the resource type breakdown from the latest completed run.
func (h *Handlers) GetResourceBreakdown(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	ctx := c.Request.Context()

	run, err := h.getLatestCompletedRun(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to get latest completed run for resource breakdown", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve resource breakdown"})
		return
	}
	if run == nil {
		c.JSON(http.StatusOK, gin.H{
			"data":    []ResourceBreakdownEntry{},
			"message": "no completed analysis runs found",
		})
		return
	}

	// Aggregate resources_by_type across all results for this run.
	breakdown, err := h.aggregateResourceTypes(ctx, run.ID, limit)
	if err != nil {
		slog.Error("Failed to aggregate resource types", "error", err, "run_id", run.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate resource types"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   breakdown,
		"run_id": run.ID,
	})
}

// GetProviderDistribution handles GET /api/v1/dashboard/providers.
// Returns provider usage data from the latest completed run.
func (h *Handlers) GetProviderDistribution(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()

	run, err := h.getLatestCompletedRun(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to get latest completed run for provider distribution", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve provider distribution"})
		return
	}
	if run == nil {
		c.JSON(http.StatusOK, gin.H{
			"data":    []ProviderDistributionEntry{},
			"message": "no completed analysis runs found",
		})
		return
	}

	// Aggregate provider_analysis across all results for this run.
	distribution, err := h.aggregateProviders(ctx, run.ID)
	if err != nil {
		slog.Error("Failed to aggregate providers", "error", err, "run_id", run.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate provider data"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   distribution,
		"run_id": run.ID,
	})
}

// GetTrends handles GET /api/v1/dashboard/trends.
// Returns historical data across multiple completed runs (last 10 by default).
func (h *Handlers) GetTrends(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	ctx := c.Request.Context()

	trends, err := h.getRunTrends(ctx, orgIDStr, limit)
	if err != nil {
		slog.Error("Failed to get run trends", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve trend data"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": trends})
}

// GetTerraformVersions handles GET /api/v1/dashboard/terraform-versions.
// Returns the Terraform version distribution from the latest completed run.
func (h *Handlers) GetTerraformVersions(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()

	run, err := h.getLatestCompletedRun(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to get latest completed run for TF versions", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve terraform version data"})
		return
	}
	if run == nil {
		c.JSON(http.StatusOK, gin.H{
			"data":    []VersionDistributionEntry{},
			"message": "no completed analysis runs found",
		})
		return
	}

	versions, err := h.aggregateTerraformVersions(ctx, run.ID)
	if err != nil {
		slog.Error("Failed to aggregate terraform versions", "error", err, "run_id", run.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to aggregate terraform versions"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   versions,
		"run_id": run.ID,
	})
}

// ---------------------------------------------------------------------------
// Data access helpers
// ---------------------------------------------------------------------------

// getLatestCompletedRun returns the most recently completed analysis run for
// the given organization, or nil if none exists.
func (h *Handlers) getLatestCompletedRun(ctx context.Context, orgID string) (*models.AnalysisRun, error) {
	var run models.AnalysisRun
	err := h.db.QueryRowContext(ctx,
		`SELECT id, organization_id, source_id, status, trigger_type, config,
		        started_at, completed_at, total_workspaces, successful_count, failed_count,
		        total_rum, total_managed, total_resources, total_data_sources,
		        error_message, performance_ms, triggered_by, created_at, updated_at
		 FROM analysis_runs
		 WHERE organization_id = $1 AND status = $2
		 ORDER BY completed_at DESC
		 LIMIT 1`,
		orgID, models.RunStatusCompleted,
	).Scan(
		&run.ID,
		&run.OrganizationID,
		&run.SourceID,
		&run.Status,
		&run.TriggerType,
		&run.Config,
		&run.StartedAt,
		&run.CompletedAt,
		&run.TotalWorkspaces,
		&run.SuccessfulCount,
		&run.FailedCount,
		&run.TotalRUM,
		&run.TotalManaged,
		&run.TotalResources,
		&run.TotalDataSources,
		&run.ErrorMessage,
		&run.PerformanceMS,
		&run.TriggeredBy,
		&run.CreatedAt,
		&run.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get latest completed run: %w", err)
	}
	return &run, nil
}

// aggregateResourceTypes aggregates the resources_by_type JSONB column across
// all results for a given run and returns the top N resource types by count.
func (h *Handlers) aggregateResourceTypes(ctx context.Context, runID string, limit int) ([]ResourceBreakdownEntry, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT resources_by_type
		 FROM analysis_results
		 WHERE run_id = $1 AND status = $2`,
		runID, models.ResultStatusSuccess,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query analysis results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	totals := make(map[string]int)
	for rows.Next() {
		var rawJSON json.RawMessage
		if err := rows.Scan(&rawJSON); err != nil {
			return nil, fmt.Errorf("failed to scan resources_by_type: %w", err)
		}
		if len(rawJSON) == 0 {
			continue
		}
		var byType map[string]int
		if err := json.Unmarshal(rawJSON, &byType); err != nil {
			continue // skip malformed JSON
		}
		for resourceType, count := range byType {
			totals[resourceType] += count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate results: %w", err)
	}

	// Sort by count descending, take top N.
	entries := make([]ResourceBreakdownEntry, 0, len(totals))
	for rt, count := range totals {
		entries = append(entries, ResourceBreakdownEntry{ResourceType: rt, Count: count})
	}
	sortResourceBreakdown(entries)

	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// aggregateProviders aggregates the provider_analysis JSONB column across
// all results for a given run and returns provider distribution data.
func (h *Handlers) aggregateProviders(ctx context.Context, runID string) ([]ProviderDistributionEntry, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT provider_analysis
		 FROM analysis_results
		 WHERE run_id = $1 AND status = $2`,
		runID, models.ResultStatusSuccess,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query analysis results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	providerResources := make(map[string]int)
	providerWorkspaces := make(map[string]int)

	for rows.Next() {
		var rawJSON json.RawMessage
		if err := rows.Scan(&rawJSON); err != nil {
			return nil, fmt.Errorf("failed to scan provider_analysis: %w", err)
		}
		if len(rawJSON) == 0 {
			continue
		}

		// The provider_analysis column stores data matching analyzer.ProviderAnalysis
		// structure or a simpler map[string]int format depending on the analysis path.
		// Try the structured format first.
		var pa struct {
			ProviderUsage map[string]struct {
				ResourceCount int `json:"resource_count"`
			} `json:"provider_usage"`
		}
		if err := json.Unmarshal(rawJSON, &pa); err == nil && len(pa.ProviderUsage) > 0 {
			for provider, usage := range pa.ProviderUsage {
				providerResources[provider] += usage.ResourceCount
				providerWorkspaces[provider]++
			}
			continue
		}

		// Fall back to simple provider -> count map (from the API handler analysis path).
		var simpleMap map[string]int
		if err := json.Unmarshal(rawJSON, &simpleMap); err == nil {
			for provider, count := range simpleMap {
				providerResources[provider] += count
				providerWorkspaces[provider]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate results: %w", err)
	}

	entries := make([]ProviderDistributionEntry, 0, len(providerResources))
	for provider, resources := range providerResources {
		entries = append(entries, ProviderDistributionEntry{
			Provider:       provider,
			ResourceCount:  resources,
			WorkspaceCount: providerWorkspaces[provider],
		})
	}
	sortProviderDistribution(entries)
	return entries, nil
}

// getRunTrends returns historical trend data from the last N completed runs.
func (h *Handlers) getRunTrends(ctx context.Context, orgID string, limit int) ([]TrendPoint, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, completed_at, total_rum, total_managed, total_resources,
		        total_workspaces, successful_count, failed_count
		 FROM analysis_runs
		 WHERE organization_id = $1 AND status = $2
		 ORDER BY completed_at DESC
		 LIMIT $3`,
		orgID, models.RunStatusCompleted, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query run trends: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var trends []TrendPoint
	for rows.Next() {
		var point TrendPoint
		var completedAt sql.NullTime
		if err := rows.Scan(
			&point.RunID,
			&completedAt,
			&point.TotalRUM,
			&point.TotalManaged,
			&point.TotalResources,
			&point.TotalWorkspaces,
			&point.SuccessfulCount,
			&point.FailedCount,
		); err != nil {
			return nil, fmt.Errorf("failed to scan trend point: %w", err)
		}
		if completedAt.Valid {
			point.CompletedAt = completedAt.Time.Format("2006-01-02T15:04:05Z")
		}
		trends = append(trends, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate trend rows: %w", err)
	}

	// Reverse so oldest is first (chronological order for charts).
	for i, j := 0, len(trends)-1; i < j; i, j = i+1, j-1 {
		trends[i], trends[j] = trends[j], trends[i]
	}

	return trends, nil
}

// aggregateTerraformVersions collects Terraform version distribution from all
// successful results of a given run.
func (h *Handlers) aggregateTerraformVersions(ctx context.Context, runID string) ([]VersionDistributionEntry, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT terraform_version, COUNT(*) as workspace_count
		 FROM analysis_results
		 WHERE run_id = $1 AND status = $2 AND terraform_version IS NOT NULL AND terraform_version != ''
		 GROUP BY terraform_version
		 ORDER BY workspace_count DESC`,
		runID, models.ResultStatusSuccess,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query terraform versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var versions []VersionDistributionEntry
	for rows.Next() {
		var entry VersionDistributionEntry
		if err := rows.Scan(&entry.Version, &entry.Count); err != nil {
			return nil, fmt.Errorf("failed to scan version entry: %w", err)
		}
		versions = append(versions, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate version rows: %w", err)
	}

	return versions, nil
}

// ---------------------------------------------------------------------------
// Organization breakdown and workspace health
// ---------------------------------------------------------------------------

// OrganizationBreakdownEntry represents aggregated metrics for a single organization.
type OrganizationBreakdownEntry struct {
	OrgName         string `json:"org_name"`
	OrgID           string `json:"org_id"`
	TotalWorkspaces int    `json:"total_workspaces"`
	TotalResources  int    `json:"total_resources"`
	TotalRUM        int    `json:"total_rum"`
}

// WorkspaceHealthEntry represents the health status of a single workspace.
type WorkspaceHealthEntry struct {
	WorkspaceName     string  `json:"workspace_name"`
	SourceName        string  `json:"source_name"`
	Status            string  `json:"status"`
	LastAnalyzed      string  `json:"last_analyzed,omitempty"`
	ResourceCount     int     `json:"resource_count"`
	RUMCount          int     `json:"rum_count"`
	DaysSinceModified *int    `json:"days_since_modified,omitempty"`
}

// GetOrganizationBreakdown handles GET /api/v1/dashboard/organizations.
// Returns aggregated analysis results grouped by organization.
func (h *Handlers) GetOrganizationBreakdown(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()

	breakdown, err := h.aggregateOrganizationBreakdown(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to aggregate organization breakdown", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve organization breakdown"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": breakdown})
}

// GetWorkspaceHealth handles GET /api/v1/dashboard/workspaces.
// Returns the latest snapshot per workspace combined with the last analysis status.
func (h *Handlers) GetWorkspaceHealth(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()

	run, err := h.getLatestCompletedRun(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to get latest completed run for workspace health", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve workspace health"})
		return
	}
	if run == nil {
		c.JSON(http.StatusOK, gin.H{
			"data":    []WorkspaceHealthEntry{},
			"message": "no completed analysis runs found",
		})
		return
	}

	health, err := h.getWorkspaceHealthEntries(ctx, orgIDStr, run.ID)
	if err != nil {
		slog.Error("Failed to get workspace health entries", "error", err, "run_id", run.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve workspace health"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   health,
		"run_id": run.ID,
	})
}

// aggregateOrganizationBreakdown aggregates analysis_results grouped by organization
// from the latest completed run for this organization.
func (h *Handlers) aggregateOrganizationBreakdown(ctx context.Context, orgID string) ([]OrganizationBreakdownEntry, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT
			COALESCE(ar.organization, $1) AS org_name,
			$1 AS org_id,
			COUNT(*) AS total_workspaces,
			COALESCE(SUM(ar.total_resources), 0) AS total_resources,
			COALESCE(SUM(ar.rum_count), 0) AS total_rum
		 FROM analysis_results ar
		 INNER JOIN analysis_runs r ON ar.run_id = r.id
		 WHERE r.organization_id = $1
		   AND r.status = $2
		   AND r.completed_at = (
		       SELECT MAX(r2.completed_at)
		       FROM analysis_runs r2
		       WHERE r2.organization_id = $1 AND r2.status = $2
		   )
		 GROUP BY COALESCE(ar.organization, $1)
		 ORDER BY total_rum DESC`,
		orgID, models.RunStatusCompleted,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query organization breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []OrganizationBreakdownEntry
	for rows.Next() {
		var entry OrganizationBreakdownEntry
		if err := rows.Scan(
			&entry.OrgName,
			&entry.OrgID,
			&entry.TotalWorkspaces,
			&entry.TotalResources,
			&entry.TotalRUM,
		); err != nil {
			return nil, fmt.Errorf("failed to scan organization breakdown entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate organization breakdown rows: %w", err)
	}

	if entries == nil {
		entries = []OrganizationBreakdownEntry{}
	}
	return entries, nil
}

// getWorkspaceHealthEntries returns per-workspace health data by joining the latest
// analysis results with snapshot and source information.
func (h *Handlers) getWorkspaceHealthEntries(ctx context.Context, orgID, runID string) ([]WorkspaceHealthEntry, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT
			ar.workspace_name,
			COALESCE(ss.name, '') AS source_name,
			ar.status,
			ar.created_at,
			ar.total_resources,
			ar.rum_count,
			ar.last_modified
		 FROM analysis_results ar
		 LEFT JOIN analysis_runs r ON ar.run_id = r.id
		 LEFT JOIN state_sources ss ON r.source_id = ss.id
		 WHERE ar.run_id = $1
		 ORDER BY ar.workspace_name`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query workspace health: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []WorkspaceHealthEntry
	now := time.Now()
	for rows.Next() {
		var entry WorkspaceHealthEntry
		var createdAt time.Time
		var lastModified sql.NullTime
		if err := rows.Scan(
			&entry.WorkspaceName,
			&entry.SourceName,
			&entry.Status,
			&createdAt,
			&entry.ResourceCount,
			&entry.RUMCount,
			&lastModified,
		); err != nil {
			return nil, fmt.Errorf("failed to scan workspace health entry: %w", err)
		}
		entry.LastAnalyzed = createdAt.Format("2006-01-02T15:04:05Z")
		if lastModified.Valid {
			days := int(now.Sub(lastModified.Time).Hours() / 24)
			entry.DaysSinceModified = &days
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate workspace health rows: %w", err)
	}

	if entries == nil {
		entries = []WorkspaceHealthEntry{}
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// Sorting helpers
// ---------------------------------------------------------------------------

// sortResourceBreakdown sorts resource breakdown entries by count descending.
func sortResourceBreakdown(entries []ResourceBreakdownEntry) {
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Count > entries[i].Count ||
				(entries[j].Count == entries[i].Count && entries[j].ResourceType < entries[i].ResourceType) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}

// sortProviderDistribution sorts provider entries by resource count descending.
func sortProviderDistribution(entries []ProviderDistributionEntry) {
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].ResourceCount > entries[i].ResourceCount ||
				(entries[j].ResourceCount == entries[i].ResourceCount && entries[j].Provider < entries[i].Provider) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}
