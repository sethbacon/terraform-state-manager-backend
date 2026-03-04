package admin

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// RoleHandlers provides HTTP handlers for role template CRUD operations.
type RoleHandlers struct {
	roleRepo *repositories.RoleTemplateRepository
}

// NewRoleHandlers creates a new RoleHandlers instance.
func NewRoleHandlers(roleRepo *repositories.RoleTemplateRepository) *RoleHandlers {
	return &RoleHandlers{
		roleRepo: roleRepo,
	}
}

// createRoleTemplateRequest represents the request body for creating a role template.
type createRoleTemplateRequest struct {
	Name        string   `json:"name" binding:"required"`
	DisplayName string   `json:"display_name" binding:"required"`
	Description *string  `json:"description"`
	Scopes      []string `json:"scopes" binding:"required"`
}

// updateRoleTemplateRequest represents the request body for updating a role template.
type updateRoleTemplateRequest struct {
	Name        *string  `json:"name"`
	DisplayName *string  `json:"display_name"`
	Description *string  `json:"description"`
	Scopes      []string `json:"scopes"`
}

// ListRoleTemplates handles listing all role templates.
func (h *RoleHandlers) ListRoleTemplates(c *gin.Context) {
	templates, err := h.roleRepo.ListRoleTemplates(c.Request.Context())
	if err != nil {
		slog.Error("failed to list role templates", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list role templates"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"role_templates": templates})
}

// GetRoleTemplate handles retrieving a single role template by its UUID.
func (h *RoleHandlers) GetRoleTemplate(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role template id is required"})
		return
	}

	template, err := h.roleRepo.GetRoleTemplateByID(c.Request.Context(), id)
	if err != nil {
		slog.Error("failed to get role template", "id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get role template"})
		return
	}
	if template == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "role template not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"role_template": template})
}

// CreateRoleTemplate handles creating a new custom role template.
// System roles cannot be created through this endpoint (is_system is always false).
func (h *RoleHandlers) CreateRoleTemplate(c *gin.Context) {
	var req createRoleTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check name uniqueness.
	existing, err := h.roleRepo.GetRoleTemplateByName(c.Request.Context(), req.Name)
	if err != nil {
		slog.Error("failed to check role template name uniqueness", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check role template name"})
		return
	}
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "a role template with this name already exists"})
		return
	}

	template := &models.RoleTemplate{
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Description: req.Description,
		Scopes:      req.Scopes,
		IsSystem:    false,
	}

	if err := h.roleRepo.CreateRoleTemplate(c.Request.Context(), template); err != nil {
		slog.Error("failed to create role template", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create role template"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"role_template": template})
}

// UpdateRoleTemplate handles updating an existing role template.
// System role templates cannot be modified.
func (h *RoleHandlers) UpdateRoleTemplate(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role template id is required"})
		return
	}

	template, err := h.roleRepo.GetRoleTemplateByID(c.Request.Context(), id)
	if err != nil {
		slog.Error("failed to get role template for update", "id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get role template"})
		return
	}
	if template == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "role template not found"})
		return
	}

	if template.IsSystem {
		c.JSON(http.StatusForbidden, gin.H{"error": "system role templates cannot be modified"})
		return
	}

	var req updateRoleTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check name uniqueness if name is being changed.
	if req.Name != nil && *req.Name != template.Name {
		existing, err := h.roleRepo.GetRoleTemplateByName(c.Request.Context(), *req.Name)
		if err != nil {
			slog.Error("failed to check role template name uniqueness", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check role template name"})
			return
		}
		if existing != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "a role template with this name already exists"})
			return
		}
		template.Name = *req.Name
	}

	if req.DisplayName != nil {
		template.DisplayName = *req.DisplayName
	}
	if req.Description != nil {
		template.Description = req.Description
	}
	if req.Scopes != nil {
		template.Scopes = req.Scopes
	}

	if err := h.roleRepo.UpdateRoleTemplate(c.Request.Context(), template); err != nil {
		slog.Error("failed to update role template", "id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update role template"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"role_template": template})
}

// DeleteRoleTemplate handles deleting a role template by ID.
// System role templates cannot be deleted.
func (h *RoleHandlers) DeleteRoleTemplate(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role template id is required"})
		return
	}

	template, err := h.roleRepo.GetRoleTemplateByID(c.Request.Context(), id)
	if err != nil {
		slog.Error("failed to get role template for deletion", "id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get role template"})
		return
	}
	if template == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "role template not found"})
		return
	}

	if template.IsSystem {
		c.JSON(http.StatusForbidden, gin.H{"error": "system role templates cannot be deleted"})
		return
	}

	if err := h.roleRepo.DeleteRoleTemplate(c.Request.Context(), id); err != nil {
		slog.Error("failed to delete role template", "id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete role template"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "role template deleted successfully"})
}
