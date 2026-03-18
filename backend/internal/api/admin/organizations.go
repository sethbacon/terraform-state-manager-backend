package admin

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// OrganizationHandlers provides HTTP handlers for organization management
// CRUD operations and member management.
type OrganizationHandlers struct {
	cfg     *config.Config
	db      *sql.DB
	orgRepo *repositories.OrganizationRepository
}

// NewOrganizationHandlers creates a new OrganizationHandlers instance.
func NewOrganizationHandlers(cfg *config.Config, db *sql.DB) *OrganizationHandlers {
	return &OrganizationHandlers{
		cfg:     cfg,
		db:      db,
		orgRepo: repositories.NewOrganizationRepository(db),
	}
}

// CreateOrganizationRequest represents the request body for creating an organization.
type CreateOrganizationRequest struct {
	Name        string  `json:"name" binding:"required"`
	DisplayName string  `json:"display_name" binding:"required"`
	Description *string `json:"description"`
}

// UpdateOrganizationRequest represents the request body for updating an organization.
type UpdateOrganizationRequest struct {
	Name        *string `json:"name"`
	DisplayName *string `json:"display_name"`
	Description *string `json:"description"`
}

// AddMemberRequest represents the request body for adding a member to an organization.
type AddMemberRequest struct {
	UserID           string `json:"user_id" binding:"required"`
	RoleTemplateName string `json:"role_template_name" binding:"required"`
}

// UpdateMemberRequest represents the request body for updating a member's role.
type UpdateMemberRequest struct {
	RoleTemplateName string `json:"role_template_name" binding:"required"`
}

// ListOrganizationsHandler returns a handler that lists organizations with pagination.
// @Summary      List organizations
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations [get]
func (h *OrganizationHandlers) ListOrganizationsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		page, perPage := parsePagination(c)
		offset := (page - 1) * perPage

		orgs, total, err := h.orgRepo.ListOrganizations(c.Request.Context(), offset, perPage)
		if err != nil {
			slog.Error("failed to list organizations", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list organizations"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"organizations": orgs,
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
				"total":    total,
			},
		})
	}
}

// GetOrganizationHandler returns a handler that retrieves a single organization
// by ID, including its members.
// @Summary      Get organization
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations/{id} [get]
func (h *OrganizationHandlers) GetOrganizationHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization id is required"})
			return
		}

		org, err := h.orgRepo.GetOrganizationByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get organization", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}

		members, err := h.orgRepo.ListMembers(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to list organization members", "org_id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list members"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"organization": org,
			"members":      members,
		})
	}
}

// CreateOrganizationHandler returns a handler that creates a new organization.
// @Summary      Create organization
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        body  body  CreateOrganizationRequest  true  "Create organization request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations [post]
func (h *OrganizationHandlers) CreateOrganizationHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateOrganizationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Check name uniqueness.
		existing, err := h.orgRepo.GetOrganizationByName(c.Request.Context(), req.Name)
		if err != nil {
			slog.Error("failed to check organization name uniqueness", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check organization name uniqueness"})
			return
		}
		if existing != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "an organization with this name already exists"})
			return
		}

		org := &models.Organization{
			Name:        req.Name,
			DisplayName: req.DisplayName,
			Description: req.Description,
			IsActive:    true,
		}

		if err := h.orgRepo.CreateOrganization(c.Request.Context(), org); err != nil {
			slog.Error("failed to create organization", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create organization"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"organization": org})
	}
}

// UpdateOrganizationHandler returns a handler that updates an existing organization.
// @Summary      Update organization
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        id    path  string                     true  "Resource ID"
// @Param        body  body  UpdateOrganizationRequest  true  "Update organization request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations/{id} [put]
func (h *OrganizationHandlers) UpdateOrganizationHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization id is required"})
			return
		}

		var req UpdateOrganizationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		org, err := h.orgRepo.GetOrganizationByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get organization for update", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}

		// Check name uniqueness if name is being changed.
		if req.Name != nil && *req.Name != org.Name {
			existing, err := h.orgRepo.GetOrganizationByName(c.Request.Context(), *req.Name)
			if err != nil {
				slog.Error("failed to check organization name uniqueness", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check organization name uniqueness"})
				return
			}
			if existing != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "an organization with this name already exists"})
				return
			}
			org.Name = *req.Name
		}

		if req.DisplayName != nil {
			org.DisplayName = *req.DisplayName
		}

		if req.Description != nil {
			org.Description = req.Description
		}

		if err := h.orgRepo.UpdateOrganization(c.Request.Context(), org); err != nil {
			slog.Error("failed to update organization", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update organization"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"organization": org})
	}
}

// DeleteOrganizationHandler returns a handler that deletes an organization by ID.
// The default organization cannot be deleted.
// @Summary      Delete organization
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      403  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations/{id} [delete]
func (h *OrganizationHandlers) DeleteOrganizationHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization id is required"})
			return
		}

		org, err := h.orgRepo.GetOrganizationByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get organization for deletion", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}

		// Prevent deleting the default organization.
		if org.Name == h.cfg.MultiTenancy.DefaultOrganization {
			c.JSON(http.StatusForbidden, gin.H{"error": "cannot delete the default organization"})
			return
		}

		if err := h.orgRepo.DeleteOrganization(c.Request.Context(), id); err != nil {
			slog.Error("failed to delete organization", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete organization"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "organization deleted successfully"})
	}
}

// SearchOrganizationsHandler returns a handler that searches organizations by query.
// Query params: q (required), page (default 1), per_page (default 20, max 100).
// @Summary      Search organizations
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        q  query  string  true  "Search query"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations/search [get]
func (h *OrganizationHandlers) SearchOrganizationsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		query := c.Query("q")
		if query == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'q' is required"})
			return
		}

		page, perPage := parsePagination(c)

		orgs, err := h.orgRepo.SearchOrganizations(c.Request.Context(), query, perPage)
		if err != nil {
			slog.Error("failed to search organizations", "query", query, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to search organizations"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"organizations": orgs,
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
				"total":    len(orgs),
			},
		})
	}
}

// ListMembersHandler returns a handler that lists all members of an organization
// with their roles.
// @Summary      List organization members
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations/{id}/members [get]
func (h *OrganizationHandlers) ListMembersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization id is required"})
			return
		}

		// Verify the organization exists.
		org, err := h.orgRepo.GetOrganizationByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get organization", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}

		members, err := h.orgRepo.ListMembers(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to list members", "org_id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list members"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"members": members})
	}
}

// AddMemberHandler returns a handler that adds a new member to an organization.
// @Summary      Add organization member
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        id    path  string            true  "Resource ID"
// @Param        body  body  AddMemberRequest  true  "Add member request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations/{id}/members [post]
func (h *OrganizationHandlers) AddMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")
		if orgID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization id is required"})
			return
		}

		var req AddMemberRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Verify the organization exists.
		org, err := h.orgRepo.GetOrganizationByID(c.Request.Context(), orgID)
		if err != nil {
			slog.Error("failed to get organization", "id", orgID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}

		// Verify the user exists.
		userRepo := repositories.NewUserRepository(h.db)
		user, err := userRepo.GetUserByID(c.Request.Context(), req.UserID)
		if err != nil {
			slog.Error("failed to get user", "user_id", req.UserID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}

		// Check if the user is already a member.
		existingMember, err := h.orgRepo.GetMember(c.Request.Context(), orgID, req.UserID)
		if err != nil {
			slog.Error("failed to check existing membership", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check membership"})
			return
		}
		if existingMember != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "user is already a member of this organization"})
			return
		}

		// Resolve the role template by name.
		roleTemplateRepo := repositories.NewRoleTemplateRepository(h.db)
		roleTemplate, err := roleTemplateRepo.GetRoleTemplateByName(c.Request.Context(), req.RoleTemplateName)
		if err != nil {
			slog.Error("failed to get role template", "name", req.RoleTemplateName, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get role template"})
			return
		}
		if roleTemplate == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "role template not found"})
			return
		}

		member := &models.OrganizationMember{
			OrganizationID: orgID,
			UserID:         req.UserID,
			RoleTemplateID: &roleTemplate.ID,
		}

		if err := h.orgRepo.AddMember(c.Request.Context(), member); err != nil {
			slog.Error("failed to add member", "org_id", orgID, "user_id", req.UserID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add member"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"member": member})
	}
}

// UpdateMemberHandler returns a handler that updates a member's role in an organization.
// @Summary      Update organization member
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        id       path  string               true  "Resource ID"
// @Param        user_id  path  string               true  "User ID"
// @Param        body     body  UpdateMemberRequest  true  "Update member request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations/{id}/members/{user_id} [put]
func (h *OrganizationHandlers) UpdateMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")
		userID := c.Param("user_id")
		if orgID == "" || userID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization id and user id are required"})
			return
		}

		var req UpdateMemberRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Verify the member exists.
		member, err := h.orgRepo.GetMember(c.Request.Context(), orgID, userID)
		if err != nil {
			slog.Error("failed to get member", "org_id", orgID, "user_id", userID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get member"})
			return
		}
		if member == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "member not found"})
			return
		}

		// Resolve the role template by name.
		roleTemplateRepo := repositories.NewRoleTemplateRepository(h.db)
		roleTemplate, err := roleTemplateRepo.GetRoleTemplateByName(c.Request.Context(), req.RoleTemplateName)
		if err != nil {
			slog.Error("failed to get role template", "name", req.RoleTemplateName, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get role template"})
			return
		}
		if roleTemplate == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "role template not found"})
			return
		}

		if err := h.orgRepo.UpdateMember(c.Request.Context(), orgID, userID, &roleTemplate.ID); err != nil {
			slog.Error("failed to update member", "org_id", orgID, "user_id", userID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update member"})
			return
		}

		// Return updated member with role info.
		updatedMember, err := h.orgRepo.GetMemberWithRole(c.Request.Context(), orgID, userID)
		if err != nil {
			slog.Error("failed to get updated member", "org_id", orgID, "user_id", userID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated member"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"member": updatedMember})
	}
}

// RemoveMemberHandler returns a handler that removes a member from an organization.
// @Summary      Remove organization member
// @Tags         Organizations
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id       path  string  true  "Resource ID"
// @Param        user_id  path  string  true  "User ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /organizations/{id}/members/{user_id} [delete]
func (h *OrganizationHandlers) RemoveMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")
		userID := c.Param("user_id")
		if orgID == "" || userID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization id and user id are required"})
			return
		}

		// Verify the member exists.
		member, err := h.orgRepo.GetMember(c.Request.Context(), orgID, userID)
		if err != nil {
			slog.Error("failed to get member for removal", "org_id", orgID, "user_id", userID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get member"})
			return
		}
		if member == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "member not found"})
			return
		}

		if err := h.orgRepo.RemoveMember(c.Request.Context(), orgID, userID); err != nil {
			slog.Error("failed to remove member", "org_id", orgID, "user_id", userID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove member"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "member removed successfully"})
	}
}
