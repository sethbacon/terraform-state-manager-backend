// Package admin implements the administrative HTTP handlers for the Terraform
// State Manager. These handlers require authentication and appropriate RBAC
// scopes. The identity surface (users, organizations, API keys, roles, OIDC,
// audit) mirrors the registry's 1:1 on the shared canonical identity model.
package admin

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// APIKeyHandlers handles API key management endpoints.
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

// apiKeyPrefix is the State Manager's API-key prefix (keys look like tsm_...).
const apiKeyPrefix = "tsm"

// CreateAPIKeyRequest represents the request to create a new API key.
type CreateAPIKeyRequest struct {
	Name           string   `json:"name" binding:"required"`
	OrganizationID string   `json:"organization_id" binding:"required"`
	Description    *string  `json:"description"`
	Scopes         []string `json:"scopes" binding:"required"`
	ExpiresAt      *string  `json:"expires_at"` // RFC3339 format
}

// CreateAPIKeyResponse represents the response when creating an API key.
type CreateAPIKeyResponse struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	Key         string     `json:"key"` // Only returned once during creation
	KeyPrefix   string     `json:"key_prefix"`
	Scopes      []string   `json:"scopes"`
	ExpiresAt   *time.Time `json:"expires_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ListAPIKeysHandler lists API keys for the authenticated user.
// If organization_id is provided, users with api_keys:manage scope see all keys
// in that org; otherwise only their own. With no org, returns the user's own keys.
// @Summary      List API keys
// @Description  List API keys with optional filtering by organization. Users with api_keys:manage scope can view all keys in an organization, otherwise only their own keys are visible.
// @Tags         API Keys
// @Accept       json
// @Produce      json
// @Param        organization_id  query  string  false  "Filter by organization ID (optional)"
// @Success      200  {object}  admin.ListAPIKeysResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /apikeys [get]
func (h *APIKeyHandlers) ListAPIKeysHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userIDVal, exists := c.Get("user_id")
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}
		userID, ok := userIDVal.(string)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user ID format"})
			return
		}

		orgID := c.Query("organization_id")

		scopesVal, _ := c.Get("scopes")
		scopes, _ := scopesVal.([]string)
		canManageAll := auth.HasScope(scopes, auth.ScopeAPIKeysManage) || auth.HasScope(scopes, auth.ScopeAdmin)

		var keys []*models.APIKey
		var err error

		if orgID != "" {
			if canManageAll {
				keys, err = h.apiKeyRepo.ListByOrganization(c.Request.Context(), orgID)
			} else {
				keys, err = h.apiKeyRepo.ListByUserAndOrganization(c.Request.Context(), userID, orgID)
			}
		} else if canManageAll {
			keys, err = h.apiKeyRepo.ListAll(c.Request.Context())
		} else {
			keys, err = h.apiKeyRepo.ListByUser(c.Request.Context(), userID)
		}

		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list API keys"})
			return
		}

		// Map to a JSON-friendly shape; never expose the key hash.
		resp := make([]gin.H, 0, len(keys))
		for _, k := range keys {
			var expiresAt interface{}
			var lastUsed interface{}
			if k.ExpiresAt != nil {
				expiresAt = k.ExpiresAt.Format(time.RFC3339)
			}
			if k.LastUsedAt != nil {
				lastUsed = k.LastUsedAt.Format(time.RFC3339)
			}
			desc := ""
			if k.Description != nil {
				desc = *k.Description
			}
			resp = append(resp, gin.H{
				"id":           k.ID,
				"user_id":      k.UserID,
				"user_name":    k.UserName,
				"name":         k.Name,
				"description":  desc,
				"key_prefix":   k.KeyPrefix,
				"scopes":       k.Scopes,
				"expires_at":   expiresAt,
				"last_used_at": lastUsed,
				"created_at":   k.CreatedAt.Format(time.RFC3339),
			})
		}

		c.JSON(http.StatusOK, gin.H{"keys": resp})
	}
}

// CreateAPIKeyHandler creates a new API key. The full key is returned only once.
// @Summary      Create API key
// @Description  Create a new API key with specified scopes. The full API key is only returned once during creation.
// @Tags         API Keys
// @Accept       json
// @Produce      json
// @Param        body  body  CreateAPIKeyRequest  true  "API key creation request"
// @Success      201  {object}  CreateAPIKeyResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /apikeys [post]
func (h *APIKeyHandlers) CreateAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateAPIKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		userIDVal, exists := c.Get("user_id")
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}
		userID, ok := userIDVal.(string)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user ID format"})
			return
		}

		if err := auth.ValidateScopes(req.Scopes); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid scopes: " + err.Error()})
			return
		}

		// Resolve organization ID — 'default' / empty resolves to the default org.
		orgID := req.OrganizationID
		if orgID == "default" || orgID == "" {
			defaultOrg, err := h.orgRepo.GetDefaultOrganization(c.Request.Context())
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get default organization"})
				return
			}
			if defaultOrg == nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Default organization not found"})
				return
			}
			orgID = defaultOrg.ID
		}

		memberWithRole, err := h.orgRepo.GetMemberWithRole(c.Request.Context(), orgID, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user role information"})
			return
		}
		if memberWithRole == nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "You are not a member of this organization"})
			return
		}
		if memberWithRole.RoleTemplateID == nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "No role template assigned for this organization. Contact an administrator to assign a role."})
			return
		}

		// Requested scopes must be within the user's role scopes (admin grants all).
		userHasAdmin := false
		for _, scope := range memberWithRole.RoleTemplateScopes {
			if scope == "admin" {
				userHasAdmin = true
				break
			}
		}
		if !userHasAdmin {
			allowedScopeSet := make(map[string]bool)
			for _, s := range memberWithRole.RoleTemplateScopes {
				allowedScopeSet[s] = true
			}
			for _, requestedScope := range req.Scopes {
				if !allowedScopeSet[requestedScope] {
					c.JSON(http.StatusForbidden, gin.H{
						"error":          "Scope '" + requestedScope + "' exceeds your role permissions for this organization",
						"allowed_scopes": memberWithRole.RoleTemplateScopes,
						"role_template":  *memberWithRole.RoleTemplateName,
					})
					return
				}
			}
		}

		var expiresAt *time.Time
		if req.ExpiresAt != nil {
			parsed, err := time.Parse(time.RFC3339, *req.ExpiresAt)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid expires_at format. Use RFC3339"})
				return
			}
			expiresAt = &parsed
		}

		fullKey, keyHash, displayPrefix, err := auth.GenerateAPIKey(apiKeyPrefix)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate API key"})
			return
		}

		apiKey := &models.APIKey{
			UserID:         &userID,
			OrganizationID: orgID,
			Name:           req.Name,
			Description:    req.Description,
			KeyHash:        keyHash,
			KeyPrefix:      displayPrefix,
			Scopes:         req.Scopes,
			ExpiresAt:      expiresAt,
			CreatedAt:      time.Now(),
		}

		if err := h.apiKeyRepo.Create(c.Request.Context(), apiKey); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create API key"})
			return
		}

		c.JSON(http.StatusCreated, CreateAPIKeyResponse{
			ID:        apiKey.ID,
			Name:      apiKey.Name,
			Key:       fullKey, // Only returned once.
			KeyPrefix: displayPrefix,
			Scopes:    apiKey.Scopes,
			ExpiresAt: apiKey.ExpiresAt,
			CreatedAt: apiKey.CreatedAt,
		})
	}
}

// GetAPIKeyHandler retrieves a specific API key by ID.
// @Summary      Get API key
// @Description  Retrieve a specific API key by ID. Users can only access their own keys unless they have admin scope.
// @Tags         API Keys
// @Accept       json
// @Produce      json
// @Param        id  path  string  true  "API key ID"
// @Success      200  {object}  admin.APIKeyResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /apikeys/{id} [get]
func (h *APIKeyHandlers) GetAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		keyID := c.Param("id")

		apiKey, err := h.apiKeyRepo.GetByID(c.Request.Context(), keyID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
			return
		}
		if apiKey == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}

		userIDVal, _ := c.Get("user_id")
		userID, _ := userIDVal.(string)
		if apiKey.UserID == nil || *apiKey.UserID != userID {
			scopesVal, _ := c.Get("scopes")
			scopes, _ := scopesVal.([]string)
			if !auth.HasScope(scopes, auth.ScopeAdmin) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
				return
			}
		}

		c.JSON(http.StatusOK, gin.H{"key": apiKey})
	}
}

// DeleteAPIKeyHandler deletes (revokes) an API key by ID.
// @Summary      Delete API key
// @Description  Delete a specific API key by ID. Users can only delete their own keys unless they have admin scope.
// @Tags         API Keys
// @Accept       json
// @Produce      json
// @Param        id  path  string  true  "API key ID"
// @Success      200  {object}  admin.MessageResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /apikeys/{id} [delete]
func (h *APIKeyHandlers) DeleteAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		keyID := c.Param("id")

		apiKey, err := h.apiKeyRepo.GetByID(c.Request.Context(), keyID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
			return
		}
		if apiKey == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}

		userIDVal, _ := c.Get("user_id")
		userID, _ := userIDVal.(string)
		if apiKey.UserID == nil || *apiKey.UserID != userID {
			scopesVal, _ := c.Get("scopes")
			scopes, _ := scopesVal.([]string)
			if !auth.HasScope(scopes, auth.ScopeAdmin) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
				return
			}
		}

		if err := h.apiKeyRepo.Delete(c.Request.Context(), keyID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete API key"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "API key deleted successfully"})
	}
}

// UpdateAPIKeyHandler updates an API key's name, scopes, or expiration.
// @Summary      Update API key
// @Description  Update an API key's name, scopes, or expiration. Users can only update their own keys unless they have admin scope.
// @Tags         API Keys
// @Accept       json
// @Produce      json
// @Param        id    path  string                  true  "API key ID"
// @Param        body  body  map[string]interface{}  true  "Update request"
// @Success      200  {object}  admin.APIKeyResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /apikeys/{id} [put]
func (h *APIKeyHandlers) UpdateAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		keyID := c.Param("id")

		var req struct {
			Name      *string  `json:"name"`
			Scopes    []string `json:"scopes"`
			ExpiresAt *string  `json:"expires_at"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		apiKey, err := h.apiKeyRepo.GetByID(c.Request.Context(), keyID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
			return
		}
		if apiKey == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}

		userIDVal, _ := c.Get("user_id")
		userID, _ := userIDVal.(string)
		if apiKey.UserID == nil || *apiKey.UserID != userID {
			scopesVal, _ := c.Get("scopes")
			scopes, _ := scopesVal.([]string)
			if !auth.HasScope(scopes, auth.ScopeAdmin) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
				return
			}
		}

		if req.Name != nil {
			apiKey.Name = *req.Name
		}

		if req.Scopes != nil {
			if err := auth.ValidateScopes(req.Scopes); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid scopes: " + err.Error()})
				return
			}
			memberWithRole, err := h.orgRepo.GetMemberWithRole(c.Request.Context(), apiKey.OrganizationID, userID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user role information"})
				return
			}
			if memberWithRole != nil && memberWithRole.RoleTemplateID != nil {
				userHasAdmin := false
				for _, scope := range memberWithRole.RoleTemplateScopes {
					if scope == "admin" {
						userHasAdmin = true
						break
					}
				}
				if !userHasAdmin {
					allowedScopeSet := make(map[string]bool)
					for _, s := range memberWithRole.RoleTemplateScopes {
						allowedScopeSet[s] = true
					}
					for _, requestedScope := range req.Scopes {
						if !allowedScopeSet[requestedScope] {
							c.JSON(http.StatusForbidden, gin.H{
								"error":          "Scope '" + requestedScope + "' exceeds your role permissions for this organization",
								"allowed_scopes": memberWithRole.RoleTemplateScopes,
								"role_template":  *memberWithRole.RoleTemplateName,
							})
							return
						}
					}
				}
			}
			apiKey.Scopes = req.Scopes
		}

		if req.ExpiresAt != nil {
			parsed, err := time.Parse(time.RFC3339, *req.ExpiresAt)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid expires_at format. Use RFC3339"})
				return
			}
			apiKey.ExpiresAt = &parsed
		}

		if err := h.apiKeyRepo.Update(c.Request.Context(), apiKey); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update API key"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"key": apiKey})
	}
}

// RotateAPIKeyRequest represents the request to rotate an API key.
type RotateAPIKeyRequest struct {
	// GracePeriodHours is how long the old key remains valid (0 = immediate revocation).
	GracePeriodHours int `json:"grace_period_hours"`
}

// RotateAPIKeyResponse represents the response when rotating an API key.
type RotateAPIKeyResponse struct {
	NewKey       CreateAPIKeyResponse `json:"new_key"`
	OldKeyStatus string               `json:"old_key_status"`
	OldExpiresAt *time.Time           `json:"old_expires_at,omitempty"`
}

// RotateAPIKeyHandler rotates an API key — creates a new key and optionally
// schedules the old key's expiration.
// @Summary      Rotate API key
// @Description  Rotate an API key by creating a new key and optionally scheduling the old key's expiration. Users can only rotate their own keys unless they have admin scope.
// @Tags         API Keys
// @Accept       json
// @Produce      json
// @Param        id    path  string               true  "API key ID"
// @Param        body  body  RotateAPIKeyRequest  true  "Rotation request"
// @Success      200  {object}  RotateAPIKeyResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /apikeys/{id}/rotate [post]
func (h *APIKeyHandlers) RotateAPIKeyHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		keyID := c.Param("id")

		var req RotateAPIKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			req.GracePeriodHours = 0
		}
		if req.GracePeriodHours < 0 || req.GracePeriodHours > 72 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "grace_period_hours must be between 0 and 72"})
			return
		}

		oldKey, err := h.apiKeyRepo.GetByID(c.Request.Context(), keyID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
			return
		}
		if oldKey == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}

		userIDVal, _ := c.Get("user_id")
		userID, _ := userIDVal.(string)
		if oldKey.UserID == nil || *oldKey.UserID != userID {
			scopesVal, _ := c.Get("scopes")
			scopes, _ := scopesVal.([]string)
			if !auth.HasScope(scopes, auth.ScopeAdmin) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
				return
			}
		}

		fullKey, keyHash, displayPrefix, err := auth.GenerateAPIKey(apiKeyPrefix)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate new API key"})
			return
		}

		newKey := &models.APIKey{
			UserID:         oldKey.UserID,
			OrganizationID: oldKey.OrganizationID,
			Name:           oldKey.Name + " (rotated)",
			Description:    oldKey.Description,
			KeyHash:        keyHash,
			KeyPrefix:      displayPrefix,
			Scopes:         oldKey.Scopes,
			ExpiresAt:      oldKey.ExpiresAt,
			CreatedAt:      time.Now(),
		}
		if err := h.apiKeyRepo.Create(c.Request.Context(), newKey); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create new API key"})
			return
		}

		var oldKeyStatus string
		var oldExpiresAt *time.Time
		if req.GracePeriodHours == 0 {
			if err := h.apiKeyRepo.Delete(c.Request.Context(), oldKey.ID); err != nil {
				oldKeyStatus = "revocation_failed"
			} else {
				oldKeyStatus = "revoked"
			}
		} else {
			gracePeriodEnd := time.Now().Add(time.Duration(req.GracePeriodHours) * time.Hour)
			oldKey.ExpiresAt = &gracePeriodEnd
			if err := h.apiKeyRepo.Update(c.Request.Context(), oldKey); err != nil {
				oldKeyStatus = "grace_period_update_failed"
			} else {
				oldKeyStatus = "expires_at"
				oldExpiresAt = &gracePeriodEnd
			}
		}

		c.JSON(http.StatusOK, RotateAPIKeyResponse{
			NewKey: CreateAPIKeyResponse{
				ID:        newKey.ID,
				Name:      newKey.Name,
				Key:       fullKey,
				KeyPrefix: displayPrefix,
				Scopes:    newKey.Scopes,
				ExpiresAt: newKey.ExpiresAt,
				CreatedAt: newKey.CreatedAt,
			},
			OldKeyStatus: oldKeyStatus,
			OldExpiresAt: oldExpiresAt,
		})
	}
}
