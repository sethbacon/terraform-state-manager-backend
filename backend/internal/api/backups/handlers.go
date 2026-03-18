// Package backups implements the HTTP handlers for state backup management,
// including backup creation, restoration, verification, and retention policies.
package backups

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/backup"
	"github.com/terraform-state-manager/terraform-state-manager/internal/validation"
)

// Handlers provides the HTTP handlers for backup API endpoints.
type Handlers struct {
	cfg           *config.Config
	db            *sql.DB
	backupSvc     *backup.Service
	retentionRepo *repositories.RetentionPolicyRepository
	backupRepo    *repositories.BackupRepository
	sourceRepo    *repositories.StateSourceRepository
	tokenCipher   *crypto.TokenCipher
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	cfg *config.Config,
	db *sql.DB,
	backupSvc *backup.Service,
	retentionRepo *repositories.RetentionPolicyRepository,
	backupRepo *repositories.BackupRepository,
	sourceRepo *repositories.StateSourceRepository,
	tokenCipher *crypto.TokenCipher,
) *Handlers {
	return &Handlers{
		cfg:           cfg,
		db:            db,
		backupSvc:     backupSvc,
		retentionRepo: retentionRepo,
		backupRepo:    backupRepo,
		sourceRepo:    sourceRepo,
		tokenCipher:   tokenCipher,
	}
}

// ---------------------------------------------------------------------------
// Backup handlers
// ---------------------------------------------------------------------------

// ListBackups handles GET /api/v1/backups.
// Returns a paginated list of state backups for the authenticated user's organization.
// @Summary      List backups
// @Description  Returns a paginated list of state backups for the organization
// @Tags         Backups
// @Produce      json
// @Param        limit   query  int  false  "Page size (default 20, max 100)"
// @Param        offset  query  int  false  "Page offset (default 0)"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups [get]
func (h *Handlers) ListBackups(c *gin.Context) {
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
	backups, total, err := h.backupRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list state backups", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list state backups"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   backups,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// CreateBackup handles POST /api/v1/backups/create.
// Creates a new state backup for the specified source and workspace. In this
// endpoint the state data is fetched from the source; the request body provides
// the source_id and workspace_name only.
// @Summary      Create backup
// @Description  Creates a new state backup for the specified source and workspace
// @Tags         Backups
// @Accept       json
// @Produce      json
// @Param        request  body      models.StateBackupCreateRequest  true  "Backup create request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/create [post]
func (h *Handlers) CreateBackup(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req models.StateBackupCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	if _, err := uuid.Parse(req.SourceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_id"})
		return
	}

	ctx := c.Request.Context()

	// Fetch the source to get connection details.
	source, err := h.sourceRepo.GetByID(ctx, req.SourceID)
	if err != nil {
		slog.Error("Failed to get source for backup", "error", err, "source_id", req.SourceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve source"})
		return
	}
	if source == nil || source.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}

	// Fetch the real state from the source backend.
	stateData, tfVersion, serial, err := h.fetchWorkspaceState(ctx, source, req.WorkspaceName)
	if err != nil {
		slog.Error("Failed to fetch state from source", "error", err,
			"source_id", req.SourceID, "workspace", req.WorkspaceName)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to fetch state: %v", err)})
		return
	}

	b, err := h.backupSvc.CreateBackup(
		ctx, orgIDStr, req.SourceID, req.WorkspaceName,
		req.WorkspaceID, stateData, tfVersion, serial,
	)
	if err != nil {
		slog.Error("Failed to create state backup", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create state backup"})
		return
	}

	slog.Info("State backup created via API",
		"backup_id", b.ID, "workspace", req.WorkspaceName,
		"size_bytes", len(stateData), "tf_version", tfVersion, "serial", serial)

	c.JSON(http.StatusCreated, gin.H{"data": b})
}

// GetBackup handles GET /api/v1/backups/:id.
// Returns metadata for a single state backup.
// @Summary      Get backup
// @Description  Returns metadata for a single state backup
// @Tags         Backups
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/{id} [get]
func (h *Handlers) GetBackup(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid backup ID"})
		return
	}

	ctx := c.Request.Context()
	b, err := h.backupRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get state backup", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state backup"})
		return
	}
	if b == nil || b.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state backup not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": b})
}

// DeleteBackup handles DELETE /api/v1/backups/:id.
// @Summary      Delete backup
// @Description  Deletes a state backup by ID
// @Tags         Backups
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/{id} [delete]
func (h *Handlers) DeleteBackup(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid backup ID"})
		return
	}

	ctx := c.Request.Context()
	b, err := h.backupRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get state backup for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state backup"})
		return
	}
	if b == nil || b.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state backup not found"})
		return
	}

	if err := h.backupSvc.DeleteBackup(ctx, id); err != nil {
		slog.Error("Failed to delete state backup", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete state backup"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "state backup deleted successfully"})
}

// RestoreBackup handles POST /api/v1/backups/:id/restore.
// Retrieves the stored state data for a backup.
// @Summary      Restore backup
// @Description  Retrieves the stored state data for a backup
// @Tags         Backups
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/{id}/restore [post]
func (h *Handlers) RestoreBackup(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid backup ID"})
		return
	}

	ctx := c.Request.Context()

	// Verify the backup belongs to the caller's organization.
	b, err := h.backupRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get state backup for restore", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state backup"})
		return
	}
	if b == nil || b.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state backup not found"})
		return
	}

	data, backup, err := h.backupSvc.RestoreBackup(ctx, id)
	if err != nil {
		slog.Error("Failed to restore state backup", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restore state backup"})
		return
	}

	slog.Info("State backup restored via API",
		"backup_id", id, "workspace", backup.WorkspaceName)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"backup":     backup,
			"state_data": string(data),
		},
	})
}

// VerifyBackup handles POST /api/v1/backups/:id/verify.
// Re-computes the SHA-256 checksum and compares it to the stored value.
// @Summary      Verify backup integrity
// @Description  Re-computes the SHA-256 checksum and compares it to the stored value
// @Tags         Backups
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/{id}/verify [post]
func (h *Handlers) VerifyBackup(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid backup ID"})
		return
	}

	ctx := c.Request.Context()

	// Verify the backup belongs to the caller's organization.
	b, err := h.backupRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get state backup for verification", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state backup"})
		return
	}
	if b == nil || b.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state backup not found"})
		return
	}

	valid, err := h.backupSvc.VerifyBackup(ctx, id)
	if err != nil {
		slog.Error("Failed to verify state backup", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify state backup"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"backup_id":       id,
			"integrity_valid": valid,
		},
	})
}

// ---------------------------------------------------------------------------
// Bulk backup
// ---------------------------------------------------------------------------

// BulkBackupRequest is the API binding for creating bulk backups.
type BulkBackupRequest struct {
	SourceID string `json:"source_id" binding:"required"`
}

// BulkBackupResult holds the summary of a bulk backup operation.
type BulkBackupResult struct {
	Total     int                    `json:"total"`
	Succeeded int                    `json:"succeeded"`
	Failed    int                    `json:"failed"`
	Backups   []BulkBackupItemResult `json:"backups"`
}

// BulkBackupItemResult describes the outcome of a single workspace backup
// within a bulk operation.
type BulkBackupItemResult struct {
	WorkspaceName string              `json:"workspace_name"`
	Status        string              `json:"status"`
	BackupID      string              `json:"backup_id,omitempty"`
	Error         string              `json:"error,omitempty"`
	Backup        *models.StateBackup `json:"backup,omitempty"`
}

// CreateBulkBackup handles POST /api/v1/backups/create-bulk.
// Lists all workspaces associated with the given source and creates an
// individual backup for each one.
// @Summary      Create bulk backup
// @Description  Creates backups for all workspaces associated with the given source
// @Tags         Backups
// @Accept       json
// @Produce      json
// @Param        request  body      BulkBackupRequest  true  "Bulk backup request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/create-bulk [post]
func (h *Handlers) CreateBulkBackup(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req BulkBackupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	if _, err := uuid.Parse(req.SourceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_id"})
		return
	}

	ctx := c.Request.Context()

	// Verify the source exists and belongs to the caller's organization.
	source, err := h.sourceRepo.GetByID(ctx, req.SourceID)
	if err != nil {
		slog.Error("Failed to get source for bulk backup", "error", err, "source_id", req.SourceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve source"})
		return
	}
	if source == nil || source.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}

	// Discover unique workspaces by querying analysis_results tied to runs for
	// this source. This gives us the known set of workspaces without requiring
	// live connectivity to the backend.
	workspaces, err := h.listWorkspacesForSource(ctx, orgIDStr, req.SourceID)
	if err != nil {
		slog.Error("Failed to list workspaces for bulk backup", "error", err, "source_id", req.SourceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workspaces for source"})
		return
	}

	if len(workspaces) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"data": BulkBackupResult{
				Total:     0,
				Succeeded: 0,
				Failed:    0,
				Backups:   []BulkBackupItemResult{},
			},
			"message": "no workspaces found for source",
		})
		return
	}

	// Create a backup for each workspace, fetching real state from the source.
	result := BulkBackupResult{
		Total:   len(workspaces),
		Backups: make([]BulkBackupItemResult, 0, len(workspaces)),
	}

	for _, ws := range workspaces {
		item := BulkBackupItemResult{WorkspaceName: ws}

		stateData, tfVersion, serial, fetchErr := h.fetchWorkspaceState(ctx, source, ws)
		if fetchErr != nil {
			item.Status = "failed"
			item.Error = fmt.Sprintf("failed to fetch state: %v", fetchErr)
			result.Failed++
			slog.Warn("Bulk backup: failed to fetch state for workspace",
				"workspace", ws, "source_id", req.SourceID, "error", fetchErr)
			result.Backups = append(result.Backups, item)
			continue
		}

		b, bErr := h.backupSvc.CreateBackup(
			ctx, orgIDStr, req.SourceID, ws, "", stateData, tfVersion, serial,
		)
		if bErr != nil {
			item.Status = "failed"
			item.Error = bErr.Error()
			result.Failed++
			slog.Warn("Bulk backup failed for workspace",
				"workspace", ws, "source_id", req.SourceID, "error", bErr)
		} else {
			item.Status = "success"
			item.BackupID = b.ID
			item.Backup = b
			result.Succeeded++
		}
		result.Backups = append(result.Backups, item)
	}

	slog.Info("Bulk backup completed",
		"source_id", req.SourceID,
		"total", result.Total,
		"succeeded", result.Succeeded,
		"failed", result.Failed)

	c.JSON(http.StatusCreated, gin.H{"data": result})
}

// listWorkspacesForSource returns distinct workspace names from analysis_results
// that are associated with analysis runs for the given source.
func (h *Handlers) listWorkspacesForSource(ctx context.Context, orgID, sourceID string) ([]string, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT DISTINCT ar.workspace_name
		 FROM analysis_results ar
		 INNER JOIN analysis_runs r ON ar.run_id = r.id
		 WHERE r.organization_id = $1 AND r.source_id = $2
		 ORDER BY ar.workspace_name`,
		orgID, sourceID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query workspaces for source: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var workspaces []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan workspace name: %w", err)
		}
		workspaces = append(workspaces, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate workspace rows: %w", err)
	}
	return workspaces, nil
}

// ---------------------------------------------------------------------------
// Retention policy handlers
// ---------------------------------------------------------------------------

// ListRetentionPolicies handles GET /api/v1/backups/retention.
// @Summary      List retention policies
// @Description  Returns all retention policies for the organization
// @Tags         Backups
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/retention [get]
func (h *Handlers) ListRetentionPolicies(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()
	policies, err := h.retentionRepo.ListByOrganization(ctx, orgIDStr)
	if err != nil {
		slog.Error("Failed to list retention policies", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list retention policies"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": policies})
}

// CreateRetentionPolicy handles POST /api/v1/backups/retention.
// @Summary      Create retention policy
// @Description  Creates a new retention policy for the organization
// @Tags         Backups
// @Accept       json
// @Produce      json
// @Param        request  body      models.RetentionPolicyCreateRequest  true  "Retention policy create request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/retention [post]
func (h *Handlers) CreateRetentionPolicy(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req models.RetentionPolicyCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	isDefault := false
	if req.IsDefault != nil {
		isDefault = *req.IsDefault
	}

	policy := &models.RetentionPolicy{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		MaxAgeDays:     req.MaxAgeDays,
		MaxCount:       req.MaxCount,
		IsDefault:      isDefault,
	}

	ctx := c.Request.Context()
	if err := h.retentionRepo.Create(ctx, policy); err != nil {
		slog.Error("Failed to create retention policy", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create retention policy"})
		return
	}

	slog.Info("Retention policy created",
		"policy_id", policy.ID, "name", policy.Name)

	c.JSON(http.StatusCreated, gin.H{"data": policy})
}

// GetRetentionPolicy handles GET /api/v1/backups/retention/:id.
// @Summary      Get retention policy
// @Description  Returns a single retention policy by ID
// @Tags         Backups
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/retention/{id} [get]
func (h *Handlers) GetRetentionPolicy(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid retention policy ID"})
		return
	}

	ctx := c.Request.Context()
	policy, err := h.retentionRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get retention policy", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve retention policy"})
		return
	}
	if policy == nil || policy.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "retention policy not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": policy})
}

// UpdateRetentionPolicy handles PUT /api/v1/backups/retention/:id.
// @Summary      Update retention policy
// @Description  Applies partial updates to a retention policy
// @Tags         Backups
// @Accept       json
// @Produce      json
// @Param        id       path  string                               true  "Resource ID"
// @Param        request  body  models.RetentionPolicyUpdateRequest  true  "Retention policy update request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/retention/{id} [put]
func (h *Handlers) UpdateRetentionPolicy(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid retention policy ID"})
		return
	}

	var req models.RetentionPolicyUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	ctx := c.Request.Context()
	policy, err := h.retentionRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get retention policy for update", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve retention policy"})
		return
	}
	if policy == nil || policy.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "retention policy not found"})
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		policy.Name = *req.Name
	}
	if req.MaxAgeDays != nil {
		policy.MaxAgeDays = req.MaxAgeDays
	}
	if req.MaxCount != nil {
		policy.MaxCount = req.MaxCount
	}
	if req.IsDefault != nil {
		policy.IsDefault = *req.IsDefault
	}

	if err := h.retentionRepo.Update(ctx, policy); err != nil {
		slog.Error("Failed to update retention policy", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update retention policy"})
		return
	}

	slog.Info("Retention policy updated", "policy_id", id)
	c.JSON(http.StatusOK, gin.H{"data": policy})
}

// DeleteRetentionPolicy handles DELETE /api/v1/backups/retention/:id.
// @Summary      Delete retention policy
// @Description  Deletes a retention policy by ID
// @Tags         Backups
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/retention/{id} [delete]
func (h *Handlers) DeleteRetentionPolicy(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid retention policy ID"})
		return
	}

	ctx := c.Request.Context()
	policy, err := h.retentionRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get retention policy for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve retention policy"})
		return
	}
	if policy == nil || policy.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "retention policy not found"})
		return
	}

	if err := h.retentionRepo.Delete(ctx, id); err != nil {
		slog.Error("Failed to delete retention policy", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete retention policy"})
		return
	}

	slog.Info("Retention policy deleted", "policy_id", id, "name", policy.Name)
	c.JSON(http.StatusOK, gin.H{"message": "retention policy deleted successfully"})
}

// ApplyRetention handles POST /api/v1/backups/retention/apply.
// Manually triggers retention policy enforcement for the organization.
// @Summary      Apply retention policies
// @Description  Manually triggers retention policy enforcement for the organization
// @Tags         Backups
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /backups/retention/apply [post]
func (h *Handlers) ApplyRetention(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	ctx := c.Request.Context()
	if err := h.backupSvc.ApplyRetention(ctx, orgIDStr); err != nil {
		slog.Error("Failed to apply retention policies", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to apply retention policies"})
		return
	}

	slog.Info("Retention policies applied", "org_id", orgIDStr)
	c.JSON(http.StatusOK, gin.H{"message": "retention policies applied successfully"})
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

// fetchWorkspaceState connects to the source backend, finds the workspace by
// name, and downloads its current state. Returns the raw state bytes,
// terraform version, and serial number.
func (h *Handlers) fetchWorkspaceState(ctx context.Context, source *models.StateSource, workspaceName string) ([]byte, string, int, error) {
	decryptedConfig, err := h.decryptConfig(source.Config)
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to decrypt source config: %w", err)
	}

	client, err := clients.NewClientFromConfig(source.SourceType, decryptedConfig)
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to create client: %w", err)
	}

	hcpAdapter, ok := client.(*clients.HCPClientAdapter)
	if !ok {
		return nil, "", 0, fmt.Errorf("backup from source type %q is not yet supported", source.SourceType)
	}

	// List workspaces for the organization and find the one matching our name.
	workspaces, err := hcpAdapter.GetWorkspaces(ctx, hcpAdapter.Organization)
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to list workspaces: %w", err)
	}

	var downloadURL string
	var wsSerial int
	var wsTFVersion string
	for _, ws := range workspaces {
		if ws.Name == workspaceName {
			downloadURL = ws.StateDownloadURL
			wsSerial = ws.StateSerial
			wsTFVersion = ws.TerraformVersion
			break
		}
	}

	if downloadURL == "" {
		return nil, "", 0, fmt.Errorf("workspace %q not found or has no state", workspaceName)
	}

	// Download the raw state bytes and parse metadata.
	stateData, stateFile, err := hcpAdapter.DownloadStateRaw(ctx, downloadURL)
	if err != nil {
		return nil, "", 0, fmt.Errorf("failed to download state: %w", err)
	}

	// Prefer metadata from the parsed state file; fall back to workspace metadata.
	tfVersion := stateFile.TerraformVersion
	if tfVersion == "" {
		tfVersion = wsTFVersion
	}
	serial := stateFile.Serial
	if serial == 0 {
		serial = wsSerial
	}

	return stateData, tfVersion, serial, nil
}
