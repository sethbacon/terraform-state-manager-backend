package admin

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// APIKeyHandlers provides HTTP handlers for API key management.
type APIKeyHandlers struct {
	cfg        *config.Config
	db         *sql.DB
	apiKeyRepo *repositories.APIKeyRepository
	orgRepo    *repositories.OrganizationRepository
	userRepo   *repositories.UserRepository
}

// NewAPIKeyHandlers creates a new APIKeyHandlers instance.
func NewAPIKeyHandlers(cfg *config.Config, db *sql.DB) *APIKeyHandlers {
	return &APIKeyHandlers{
		cfg:        cfg,
		db:         db,
		apiKeyRepo: repositories.NewAPIKeyRepository(db),
		orgRepo:    repositories.NewOrganizationRepository(db),
		userRepo:   repositories.NewUserRepository(db),
	}
}

// CreateAPIKeyRequest represents the request body for creating a new API key.
type CreateAPIKeyRequest struct {
	Name           string     `json:"name" binding:"required"`
	Description    *string    `json:"description"`
	OrganizationID string     `json:"organization_id" binding:"required"`
	Scopes         []string   `json:"scopes" binding:"required"`
	ExpiresAt      *time.Time `json:"expires_at"`
}

// CreateAPIKeyResponse represents the response when creating a new API key.
// The full key value is returned only once at creation time.
type CreateAPIKeyResponse struct {
	Key       string         `json:"key"`
	KeyPrefix string         `json:"key_prefix"`
	APIKey    *models.APIKey `json:"api_key"`
}

// RotateAPIKeyRequest represents the request body for rotating an API key.
type RotateAPIKeyRequest struct {
	GracePeriodHours int `json:"grace_period_hours"`
}

// RotateAPIKeyResponse represents the response when rotating an API key.
type RotateAPIKeyResponse struct {
	Key             string         `json:"key"`
	KeyPrefix       string         `json:"key_prefix"`
	APIKey          *models.APIKey `json:"api_key"`
	OldKeyExpiresAt *time.Time     `json:"old_key_expires_at,omitempty"`
}

// ListAPIKeysHandler returns a handler that lists API keys.
// If the user has the api_keys:manage scope, all keys for the organization are
// returned. Otherwise only keys owned by the current user are returned.
// @Summary      List API keys
// @Tags         API Keys
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        organization_id  query  string  false  "Filter by organization ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /apikeys [get]
func (h *APIKeyHandlers) ListAPIKeysHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, _ := c.Get("user_id")
		uid, _ := userID.(string)
		scopes, _ := c.Get("scopes")
		userScopes, _ := scopes.([]string)

		// If the user has api_keys:manage scope, optionally filter by org.
		// Otherwise only show the user's own keys.
		if auth.HasScope(userScopes, auth.ScopeAPIKeysManage) {
			orgID := c.Query("organization_id")
			if orgID != "" {
				keys, err := h.listAPIKeysByOrg(c, orgID)
				if err != nil {
					slog.Error("failed to list API keys by org", "org_id", orgID, "error", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list API keys"})
					return
				}
				c.JSON(http.StatusOK, gin.H{"api_keys": keys})
				return
			}
			// Return all keys (no filter) via a broader query.
			keys, err := h.listAllAPIKeys(c)
			if err != nil {
				slog.Error("failed to list all API keys", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list API keys"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"api_keys": keys})
			return
		}

		// Non-admin: list only the current user's keys.
		if uid == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
			return
		}

		keys, err := h.apiKeyRepo.ListByUser(c.Request.Context(), uid)
		if err != nil {
			slog.Error("failed to list user API keys", "user_id", uid, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list API keys"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"api_keys": keys})
	}
}

// CreateAPIKeyHandler returns a handler that creates a new API key.
// It validates the requested scopes, verifies organization membership and role
// permissions, and returns the full key value only once.
// @Summary      Create API key
// @Tags         API Keys
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        body  body  CreateAPIKeyRequest  true  "Create API key request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /apikeys [post]
func (h *APIKeyHandlers) CreateAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateAPIKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		userID, _ := c.Get("user_id")
		uid, _ := userID.(string)
		if uid == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
			return
		}

		// Validate all requested scopes.
		if err := auth.ValidateScopes(req.Scopes); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Verify the organization exists.
		org, err := h.orgRepo.GetByID(c.Request.Context(), req.OrganizationID)
		if err != nil {
			slog.Error("failed to get organization", "id", req.OrganizationID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization not found"})
			return
		}

		// Verify user is a member of the organization.
		member, err := h.orgRepo.GetMemberWithRole(c.Request.Context(), req.OrganizationID, uid)
		if err != nil {
			slog.Error("failed to check org membership", "org_id", req.OrganizationID, "user_id", uid, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify membership"})
			return
		}

		// Allow admins to create keys even without explicit membership.
		scopes, _ := c.Get("scopes")
		userScopes, _ := scopes.([]string)
		isAdmin := auth.HasScope(userScopes, auth.ScopeAdmin)

		if member == nil && !isAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "you are not a member of this organization"})
			return
		}

		// Verify the user's role permits the requested scopes (unless admin).
		if !isAdmin && member != nil {
			for _, requestedScope := range req.Scopes {
				if !auth.HasScope(member.RoleTemplateScopes, auth.Scope(requestedScope)) {
					c.JSON(http.StatusForbidden, gin.H{
						"error": "your role does not permit the scope: " + requestedScope,
					})
					return
				}
			}
		}

		// Generate the API key.
		fullKey, keyHash, keyPrefix, err := auth.GenerateAPIKey("tsm")
		if err != nil {
			slog.Error("failed to generate API key", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate API key"})
			return
		}

		apiKey := &models.APIKey{
			UserID:         &uid,
			OrganizationID: req.OrganizationID,
			Name:           req.Name,
			Description:    req.Description,
			KeyHash:        keyHash,
			KeyPrefix:      keyPrefix,
			Scopes:         req.Scopes,
			ExpiresAt:      req.ExpiresAt,
		}

		if err := h.apiKeyRepo.CreateAPIKey(c.Request.Context(), apiKey); err != nil {
			slog.Error("failed to create API key", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create API key"})
			return
		}

		c.JSON(http.StatusCreated, CreateAPIKeyResponse{
			Key:       fullKey,
			KeyPrefix: keyPrefix,
			APIKey:    apiKey,
		})
	}
}

// GetAPIKeyHandler returns a handler that retrieves a single API key by ID.
// Users can retrieve their own keys; users with api_keys:manage scope can
// retrieve any key.
// @Summary      Get API key
// @Tags         API Keys
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /apikeys/{id} [get]
func (h *APIKeyHandlers) GetAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "API key id is required"})
			return
		}

		apiKey, err := h.apiKeyRepo.GetAPIKeyByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get API key", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get API key"})
			return
		}
		if apiKey == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}

		// Check ownership or admin scope.
		userID, _ := c.Get("user_id")
		uid, _ := userID.(string)
		scopes, _ := c.Get("scopes")
		userScopes, _ := scopes.([]string)

		isOwner := apiKey.UserID != nil && *apiKey.UserID == uid
		isAdmin := auth.HasScope(userScopes, auth.ScopeAPIKeysManage)

		if !isOwner && !isAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "you do not have permission to view this API key"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"api_key": apiKey})
	}
}

// UpdateAPIKeyHandler returns a handler that updates an existing API key.
// Only the key owner can update it. Validates scopes on update.
// @Summary      Update API key
// @Tags         API Keys
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        id    path  string                  true  "Resource ID"
// @Param        body  body  map[string]interface{}  true  "Update API key request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /apikeys/{id} [put]
func (h *APIKeyHandlers) UpdateAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "API key id is required"})
			return
		}

		apiKey, err := h.apiKeyRepo.GetAPIKeyByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get API key for update", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get API key"})
			return
		}
		if apiKey == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}

		// Check ownership.
		userID, _ := c.Get("user_id")
		uid, _ := userID.(string)
		scopes, _ := c.Get("scopes")
		userScopes, _ := scopes.([]string)

		isOwner := apiKey.UserID != nil && *apiKey.UserID == uid
		isAdmin := auth.HasScope(userScopes, auth.ScopeAPIKeysManage)

		if !isOwner && !isAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "you do not have permission to update this API key"})
			return
		}

		var updateReq struct {
			Name        *string    `json:"name"`
			Description *string    `json:"description"`
			Scopes      []string   `json:"scopes"`
			ExpiresAt   *time.Time `json:"expires_at"`
		}
		if err := c.ShouldBindJSON(&updateReq); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if updateReq.Scopes != nil {
			if err := auth.ValidateScopes(updateReq.Scopes); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			apiKey.Scopes = updateReq.Scopes
		}

		if updateReq.Name != nil {
			apiKey.Name = *updateReq.Name
		}
		if updateReq.Description != nil {
			apiKey.Description = updateReq.Description
		}
		if updateReq.ExpiresAt != nil {
			apiKey.ExpiresAt = updateReq.ExpiresAt
		}

		if err := h.apiKeyRepo.Update(c.Request.Context(), apiKey); err != nil {
			slog.Error("failed to update API key", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update API key"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"api_key": apiKey})
	}
}

// DeleteAPIKeyHandler returns a handler that deletes an API key by ID.
// The key owner or a user with api_keys:manage scope can delete a key.
// @Summary      Delete API key
// @Tags         API Keys
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /apikeys/{id} [delete]
func (h *APIKeyHandlers) DeleteAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "API key id is required"})
			return
		}

		apiKey, err := h.apiKeyRepo.GetAPIKeyByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get API key for deletion", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get API key"})
			return
		}
		if apiKey == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}

		// Check ownership or admin.
		userID, _ := c.Get("user_id")
		uid, _ := userID.(string)
		scopes, _ := c.Get("scopes")
		userScopes, _ := scopes.([]string)

		isOwner := apiKey.UserID != nil && *apiKey.UserID == uid
		isAdmin := auth.HasScope(userScopes, auth.ScopeAPIKeysManage)

		if !isOwner && !isAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "you do not have permission to delete this API key"})
			return
		}

		if err := h.apiKeyRepo.Delete(c.Request.Context(), id); err != nil {
			slog.Error("failed to delete API key", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete API key"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "API key deleted successfully"})
	}
}

// RotateAPIKeyHandler returns a handler that rotates an existing API key.
// A new key is generated with the same properties as the old key. An optional
// grace period (0-72 hours) may be specified during which the old key remains
// valid.
// @Summary      Rotate API key
// @Tags         API Keys
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        id    path  string               true  "Resource ID"
// @Param        body  body  RotateAPIKeyRequest  true  "Rotate API key request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /apikeys/{id}/rotate [post]
func (h *APIKeyHandlers) RotateAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "API key id is required"})
			return
		}

		apiKey, err := h.apiKeyRepo.GetAPIKeyByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get API key for rotation", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get API key"})
			return
		}
		if apiKey == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}

		// Check ownership or admin.
		userID, _ := c.Get("user_id")
		uid, _ := userID.(string)
		scopes, _ := c.Get("scopes")
		userScopes, _ := scopes.([]string)

		isOwner := apiKey.UserID != nil && *apiKey.UserID == uid
		isAdmin := auth.HasScope(userScopes, auth.ScopeAPIKeysManage)

		if !isOwner && !isAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "you do not have permission to rotate this API key"})
			return
		}

		var req RotateAPIKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Validate grace period: 0..72 hours.
		if req.GracePeriodHours < 0 || req.GracePeriodHours > 72 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "grace_period_hours must be between 0 and 72"})
			return
		}

		// Generate new key credentials.
		fullKey, newHash, newPrefix, err := auth.GenerateAPIKey("tsm")
		if err != nil {
			slog.Error("failed to generate rotated API key", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate new API key"})
			return
		}

		var oldKeyExpiresAt *time.Time

		if req.GracePeriodHours > 0 {
			// During the grace period, create a new key entry with the same
			// properties and deactivate the old key after the grace period.
			graceDuration := time.Duration(req.GracePeriodHours) * time.Hour
			expiresAt := time.Now().Add(graceDuration)
			oldKeyExpiresAt = &expiresAt

			// Set the old key to expire after the grace period.
			apiKey.ExpiresAt = oldKeyExpiresAt
			if err := h.apiKeyRepo.Update(c.Request.Context(), apiKey); err != nil {
				slog.Error("failed to set grace period on old key", "id", id, "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to set grace period"})
				return
			}

			// Create the new key as a separate entry.
			newAPIKey := &models.APIKey{
				UserID:         apiKey.UserID,
				OrganizationID: apiKey.OrganizationID,
				Name:           apiKey.Name,
				Description:    apiKey.Description,
				KeyHash:        newHash,
				KeyPrefix:      newPrefix,
				Scopes:         apiKey.Scopes,
				ExpiresAt:      nil, // The new key does not inherit the old expiry.
			}

			if err := h.apiKeyRepo.CreateAPIKey(c.Request.Context(), newAPIKey); err != nil {
				slog.Error("failed to create rotated API key", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create rotated API key"})
				return
			}

			c.JSON(http.StatusOK, RotateAPIKeyResponse{
				Key:             fullKey,
				KeyPrefix:       newPrefix,
				APIKey:          newAPIKey,
				OldKeyExpiresAt: oldKeyExpiresAt,
			})
			return
		}

		// No grace period: rotate the key in-place (swap the stored credentials).
		if _, err := h.db.ExecContext(c.Request.Context(),
			`UPDATE api_keys SET key_hash = $2, key_prefix = $3 WHERE id = $1`,
			id, newHash, newPrefix,
		); err != nil {
			slog.Error("failed to rotate API key", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rotate API key"})
			return
		}

		// Refresh the key after rotation.
		rotatedKey, err := h.apiKeyRepo.GetAPIKeyByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get rotated API key", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get rotated API key"})
			return
		}

		c.JSON(http.StatusOK, RotateAPIKeyResponse{
			Key:       fullKey,
			KeyPrefix: newPrefix,
			APIKey:    rotatedKey,
		})
	}
}

// listAPIKeysByOrg queries API keys filtered by organization.
func (h *APIKeyHandlers) listAPIKeysByOrg(c *gin.Context, orgID string) ([]*models.APIKey, error) {
	return h.apiKeyRepo.ListByOrganization(c.Request.Context(), orgID)
}

// listAllAPIKeys returns all API keys across all organizations.
func (h *APIKeyHandlers) listAllAPIKeys(c *gin.Context) ([]*models.APIKey, error) {
	return h.apiKeyRepo.ListAll(c.Request.Context())
}
