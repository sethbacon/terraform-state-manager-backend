// Package sources implements the HTTP handlers for state source management
// (CRUD operations, connectivity testing).
package sources

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/validation"
)

// Handlers provides the HTTP handlers for state source API endpoints.
type Handlers struct {
	cfg         *config.Config
	sourceRepo  *repositories.StateSourceRepository
	tokenCipher *crypto.TokenCipher
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	cfg *config.Config,
	sourceRepo *repositories.StateSourceRepository,
	tokenCipher *crypto.TokenCipher,
) *Handlers {
	return &Handlers{
		cfg:         cfg,
		sourceRepo:  sourceRepo,
		tokenCipher: tokenCipher,
	}
}

// ---------------------------------------------------------------------------
// Config encryption helpers
// ---------------------------------------------------------------------------

// encryptConfig encrypts sensitive values within a JSON config map. Keys whose
// names match known sensitive patterns (token, key, secret, etc.) have their
// plaintext values replaced with AES-256-GCM ciphertext.
func (h *Handlers) encryptConfig(rawConfig json.RawMessage) (json.RawMessage, error) {
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
		encrypted, err := h.tokenCipher.Seal(str)
		if err != nil {
			return nil, err
		}
		configMap[k] = encrypted
	}

	return json.Marshal(configMap)
}

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
			// Value was not encrypted or cannot be decrypted; keep as-is.
			slog.Warn("Failed to decrypt config field",
				"key", k, "error", err)
			continue
		}
		configMap[k] = decrypted
	}

	return json.Marshal(configMap)
}

// maskConfig replaces sensitive config values with a masked placeholder for
// safe API responses.
func maskConfig(rawConfig json.RawMessage) json.RawMessage {
	var configMap map[string]interface{}
	if err := json.Unmarshal(rawConfig, &configMap); err != nil {
		return rawConfig
	}

	for k, v := range configMap {
		if !validation.IsSensitiveConfigKey(k) {
			continue
		}
		str, ok := v.(string)
		if !ok || str == "" {
			continue
		}
		if len(str) <= 6 {
			configMap[k] = "***MASKED***"
		} else {
			configMap[k] = str[:3] + "***MASKED***" + str[len(str)-3:]
		}
	}

	masked, err := json.Marshal(configMap)
	if err != nil {
		return rawConfig
	}
	return masked
}

// ---------------------------------------------------------------------------
// Handler methods
// ---------------------------------------------------------------------------

// ListSources handles GET /api/v1/sources.
// Returns a paginated list of state sources for the authenticated user's
// organization. Query parameters: limit (default 20), offset (default 0).
func (h *Handlers) ListSources(c *gin.Context) {
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
	sources, total, err := h.sourceRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list state sources", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list state sources"})
		return
	}

	// Mask sensitive config fields before returning.
	type sourceResp struct {
		models.StateSource
		Config json.RawMessage `json:"config"`
	}
	items := make([]sourceResp, 0, len(sources))
	for _, s := range sources {
		items = append(items, sourceResp{
			StateSource: s,
			Config:      maskConfig(s.Config),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   items,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// CreateSource handles POST /api/v1/sources.
// Binds a StateSourceCreateRequest, encrypts sensitive config fields, and
// persists the new source.
func (h *Handlers) CreateSource(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)

	var req models.StateSourceCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	// Encrypt sensitive config fields before storage.
	encryptedConfig, err := h.encryptConfig(req.Config)
	if err != nil {
		slog.Error("Failed to encrypt source config", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt source configuration"})
		return
	}

	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	source := &models.StateSource{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		SourceType:     req.SourceType,
		Config:         encryptedConfig,
		IsActive:       isActive,
		CreatedBy:      nilIfEmpty(userIDStr),
	}

	ctx := c.Request.Context()
	if err := h.sourceRepo.Create(ctx, source); err != nil {
		slog.Error("Failed to create state source", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create state source"})
		return
	}

	slog.Info("State source created",
		"source_id", source.ID, "name", source.Name, "type", source.SourceType)

	// Mask config in the response.
	source.Config = maskConfig(source.Config)

	c.JSON(http.StatusCreated, gin.H{"data": source})
}

// GetSource handles GET /api/v1/sources/:id.
// Returns a single source by ID, masking sensitive config fields.
func (h *Handlers) GetSource(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source ID"})
		return
	}

	ctx := c.Request.Context()
	source, err := h.sourceRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get state source", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state source"})
		return
	}
	if source == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "state source not found"})
		return
	}

	// Verify the source belongs to the caller's organization.
	if source.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state source not found"})
		return
	}

	source.Config = maskConfig(source.Config)
	c.JSON(http.StatusOK, gin.H{"data": source})
}

// UpdateSource handles PUT /api/v1/sources/:id.
// Applies partial updates to a state source. Sensitive config fields in the
// update payload are encrypted before storage.
func (h *Handlers) UpdateSource(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source ID"})
		return
	}

	var req models.StateSourceUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	ctx := c.Request.Context()
	source, err := h.sourceRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get state source for update", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state source"})
		return
	}
	if source == nil || source.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state source not found"})
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		source.Name = *req.Name
	}
	if req.SourceType != nil {
		source.SourceType = *req.SourceType
	}
	if req.IsActive != nil {
		source.IsActive = *req.IsActive
	}
	if req.Config != nil {
		encryptedConfig, err := h.encryptConfig(*req.Config)
		if err != nil {
			slog.Error("Failed to encrypt updated source config", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt source configuration"})
			return
		}
		source.Config = encryptedConfig
	}

	if err := h.sourceRepo.Update(ctx, source); err != nil {
		slog.Error("Failed to update state source", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update state source"})
		return
	}

	slog.Info("State source updated", "source_id", id)

	source.Config = maskConfig(source.Config)
	c.JSON(http.StatusOK, gin.H{"data": source})
}

// DeleteSource handles DELETE /api/v1/sources/:id.
func (h *Handlers) DeleteSource(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source ID"})
		return
	}

	ctx := c.Request.Context()
	source, err := h.sourceRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get state source for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state source"})
		return
	}
	if source == nil || source.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state source not found"})
		return
	}

	if err := h.sourceRepo.Delete(ctx, id); err != nil {
		slog.Error("Failed to delete state source", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete state source"})
		return
	}

	slog.Info("State source deleted", "source_id", id, "name", source.Name)
	c.JSON(http.StatusOK, gin.H{
		"message": "state source deleted successfully",
	})
}

// TestSource handles POST /api/v1/sources/:id/test.
// Decrypts the source configuration, creates the appropriate backend client,
// tests the connection, and updates the source's test status fields.
func (h *Handlers) TestSource(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source ID"})
		return
	}

	ctx := c.Request.Context()

	// 1. Load the source from DB.
	source, err := h.sourceRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get state source for testing", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve state source"})
		return
	}
	if source == nil || source.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "state source not found"})
		return
	}

	// 2. Decrypt sensitive config fields.
	decryptedConfig, err := h.decryptConfig(source.Config)
	if err != nil {
		slog.Error("Failed to decrypt source config", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt source configuration"})
		return
	}

	// 3. Create the appropriate client based on source_type.
	client, err := clients.NewClientFromConfig(source.SourceType, decryptedConfig)
	if err != nil {
		slog.Warn("Failed to create client for source test",
			"error", err, "source_id", id, "source_type", source.SourceType)
		status := "failed"
		now := time.Now()
		_ = h.sourceRepo.UpdateTestStatus(ctx, id, status, now)
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "failed to create client",
			"detail": err.Error(),
		})
		return
	}

	// 4. Test the connection.
	testErr := client.TestConnection(ctx)

	// 5. Update the source's test status in DB.
	now := time.Now()
	status := "success"
	if testErr != nil {
		status = "failed"
	}
	if updateErr := h.sourceRepo.UpdateTestStatus(ctx, id, status, now); updateErr != nil {
		slog.Error("Failed to update source test status",
			"error", updateErr, "source_id", id)
	}

	// 6. Return success or failure.
	if testErr != nil {
		slog.Info("Source connectivity test failed",
			"source_id", id, "error", testErr)
		c.JSON(http.StatusOK, gin.H{
			"success":     false,
			"status":      status,
			"tested_at":   now,
			"error":       testErr.Error(),
			"source_id":   id,
			"source_type": source.SourceType,
		})
		return
	}

	slog.Info("Source connectivity test succeeded",
		"source_id", id, "source_type", source.SourceType)
	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"status":      status,
		"tested_at":   now,
		"source_id":   id,
		"source_type": source.SourceType,
	})
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
