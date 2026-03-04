// Package migrations implements the HTTP handlers for storage migration
// management, including job creation, execution, validation, and dry-run.
package migrations

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/migration"
)

// Handlers provides the HTTP handlers for migration API endpoints.
type Handlers struct {
	cfg           *config.Config
	migrationSvc  *migration.Service
	migrationRepo *repositories.MigrationRepository
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	cfg *config.Config,
	migrationSvc *migration.Service,
	migrationRepo *repositories.MigrationRepository,
) *Handlers {
	return &Handlers{
		cfg:           cfg,
		migrationSvc:  migrationSvc,
		migrationRepo: migrationRepo,
	}
}

// ---------------------------------------------------------------------------
// Handler methods
// ---------------------------------------------------------------------------

// CreateMigration handles POST /api/v1/migrations.
// Creates a new migration job and optionally starts execution.
func (h *Handlers) CreateMigration(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)

	var req models.MigrationJobCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	dryRun := false
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	errorLog, _ := json.Marshal([]interface{}{})

	job := &models.MigrationJob{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		SourceBackend:  req.SourceBackend,
		SourceConfig:   req.SourceConfig,
		TargetBackend:  req.TargetBackend,
		TargetConfig:   req.TargetConfig,
		DryRun:         dryRun,
		ErrorLog:       errorLog,
		CreatedBy:      nilIfEmpty(userIDStr),
	}

	ctx := c.Request.Context()
	if err := h.migrationSvc.CreateJob(ctx, job); err != nil {
		slog.Error("Failed to create migration job", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Auto-start the migration job unless it is a dry run.
	if !dryRun {
		if err := h.migrationSvc.ExecuteJob(ctx, job.ID); err != nil {
			slog.Warn("Failed to auto-start migration job",
				"job_id", job.ID, "error", err)
			// Job was created, just not started; return 201 anyway.
		}
	}

	slog.Info("Migration job created via API",
		"job_id", job.ID, "name", job.Name)

	c.JSON(http.StatusCreated, gin.H{"data": job})
}

// ListMigrations handles GET /api/v1/migrations.
// Returns a paginated list of migration jobs for the organization.
func (h *Handlers) ListMigrations(c *gin.Context) {
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
	jobs, total, err := h.migrationRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list migration jobs", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list migration jobs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   jobs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetMigration handles GET /api/v1/migrations/:id.
// Returns the details of a single migration job.
func (h *Handlers) GetMigration(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid migration job ID"})
		return
	}

	ctx := c.Request.Context()
	job, err := h.migrationRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get migration job", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve migration job"})
		return
	}
	if job == nil || job.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "migration job not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": job})
}

// CancelMigration handles POST /api/v1/migrations/:id/cancel.
// Cancels a pending or running migration job.
func (h *Handlers) CancelMigration(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid migration job ID"})
		return
	}

	ctx := c.Request.Context()

	// Verify the job belongs to the caller's organization.
	job, err := h.migrationRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get migration job for cancellation", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve migration job"})
		return
	}
	if job == nil || job.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "migration job not found"})
		return
	}

	if err := h.migrationSvc.CancelJob(ctx, id); err != nil {
		slog.Error("Failed to cancel migration job", "error", err, "id", id)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	slog.Info("Migration job cancelled via API", "job_id", id)
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":     id,
			"status": models.MigrationStatusCancelled,
		},
		"message": "migration job cancelled",
	})
}

// ValidateMigration handles POST /api/v1/migrations/validate.
// Validates a migration configuration without creating a job.
func (h *Handlers) ValidateMigration(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req models.MigrationJobCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	job := &models.MigrationJob{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		SourceBackend:  req.SourceBackend,
		SourceConfig:   req.SourceConfig,
		TargetBackend:  req.TargetBackend,
		TargetConfig:   req.TargetConfig,
	}

	ctx := c.Request.Context()
	if err := h.migrationSvc.ValidateJob(ctx, job); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"valid":  false,
			"errors": []string{err.Error()},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"valid":  true,
		"errors": []string{},
	})
}

// DryRunMigration handles POST /api/v1/migrations/dry-run.
// Simulates a migration and returns what would be migrated.
func (h *Handlers) DryRunMigration(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req models.MigrationJobCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	job := &models.MigrationJob{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		SourceBackend:  req.SourceBackend,
		SourceConfig:   req.SourceConfig,
		TargetBackend:  req.TargetBackend,
		TargetConfig:   req.TargetConfig,
		DryRun:         true,
	}

	ctx := c.Request.Context()
	result, err := h.migrationSvc.DryRun(ctx, job)
	if err != nil {
		slog.Error("Failed to perform dry-run migration", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": result})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nilIfEmpty returns a pointer to s if non-empty, otherwise nil.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
