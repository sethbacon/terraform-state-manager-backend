// organizations.go implements handlers for organization CRUD operations and
// membership management. Mirrors the registry's identity surface 1:1 on the
// shared canonical identity model.
package admin

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// OrganizationHandlers handles organization management endpoints.
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

// ListOrganizationsHandler lists all organizations with pagination.
// @Summary      List organizations
// @Description  Get a paginated list of all organizations.
// @Tags         Organizations
// @Produce      json
// @Param        page      query  int  false  "Page number (default 1)"
// @Param        per_page  query  int  false  "Items per page, max 100 (default 20)"
// @Success      200  {object}  admin.ListOrganizationsResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations [get]
func (h *OrganizationHandlers) ListOrganizationsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "20"))
		if page < 1 {
			page = 1
		}
		if perPage < 1 || perPage > 100 {
			perPage = 20
		}
		offset := (page - 1) * perPage

		orgs, err := h.orgRepo.List(c.Request.Context(), perPage, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list organizations"})
			return
		}
		total, err := h.orgRepo.Count(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to count organizations"})
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

// GetOrganizationHandler retrieves a specific organization by ID with members.
// @Summary      Get organization
// @Description  Retrieve a specific organization by its ID, including member list.
// @Tags         Organizations
// @Produce      json
// @Param        id  path  string  true  "Organization ID"
// @Success      200  {object}  admin.OrganizationWithMembersResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations/{id} [get]
func (h *OrganizationHandlers) GetOrganizationHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")

		org, err := h.orgRepo.GetByID(c.Request.Context(), orgID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Organization not found"})
			return
		}

		members, err := h.orgRepo.ListMembersWithUsers(c.Request.Context(), orgID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve organization members"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"organization": org,
			"members":      members,
		})
	}
}

// ListMembersHandler retrieves all members of an organization with user details.
// @Summary      List organization members
// @Description  Retrieve all members of a specific organization including user details.
// @Tags         Organizations
// @Produce      json
// @Param        id  path  string  true  "Organization ID"
// @Success      200  {object}  admin.OrganizationMembersResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations/{id}/members [get]
func (h *OrganizationHandlers) ListMembersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")

		org, err := h.orgRepo.GetByID(c.Request.Context(), orgID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Organization not found"})
			return
		}

		members, err := h.orgRepo.ListMembersWithUsers(c.Request.Context(), orgID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve organization members"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"members": members})
	}
}

// CreateOrganizationRequest represents the request to create a new organization.
type CreateOrganizationRequest struct {
	Name        string `json:"name" binding:"required"`
	DisplayName string `json:"display_name" binding:"required"`
}

// CreateOrganizationHandler creates a new organization.
// @Summary      Create organization
// @Description  Create a new organization.
// @Tags         Organizations
// @Accept       json
// @Produce      json
// @Param        body  body  CreateOrganizationRequest  true  "Organization name and display name"
// @Success      201  {object}  admin.OrganizationResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations [post]
func (h *OrganizationHandlers) CreateOrganizationHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateOrganizationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
			return
		}

		existingOrg, err := h.orgRepo.GetByName(c.Request.Context(), req.Name)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check existing organization"})
			return
		}
		if existingOrg != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Organization with this name already exists"})
			return
		}

		org := &models.Organization{
			Name:        req.Name,
			DisplayName: req.DisplayName,
		}
		if err := h.orgRepo.Create(c.Request.Context(), org); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create organization"})
			return
		}

		// Auto-add the creating user as an admin member so they can access the org immediately.
		if rawUID, exists := c.Get("user_id"); exists {
			if uid, ok := rawUID.(string); ok && uid != "" {
				_ = h.orgRepo.AddMemberWithParams(c.Request.Context(), org.ID, uid, "admin")
			}
		}

		c.JSON(http.StatusCreated, gin.H{"organization": org})
	}
}

// UpdateOrganizationRequest represents the request to update an organization.
type UpdateOrganizationRequest struct {
	Name        *string `json:"name"`         // Optional rename
	DisplayName *string `json:"display_name"` // Human-readable display name
	IdpType     *string `json:"idp_type"`     // "oidc", "saml", "ldap", or empty to clear
	IdpName     *string `json:"idp_name"`     // IdP name within type, or null to clear
}

// UpdateOrganizationHandler updates an organization's name, display name, and IdP binding.
// @Summary      Update organization
// @Description  Update an existing organization's name, display name, and optional IdP binding. Memberships reference the organization by UUID and are unaffected by a rename. Set idp_type to "oidc", "saml", or "ldap" to restrict login; set to empty string to clear.
// @Tags         Organizations
// @Accept       json
// @Produce      json
// @Param        id    path  string                     true  "Organization ID"
// @Param        body  body  UpdateOrganizationRequest  true  "Fields to update"
// @Success      200  {object}  admin.OrganizationResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations/{id} [put]
func (h *OrganizationHandlers) UpdateOrganizationHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")

		var req UpdateOrganizationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
			return
		}

		org, err := h.orgRepo.GetByID(c.Request.Context(), orgID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Organization not found"})
			return
		}

		// Handle rename — check uniqueness, then rename the identity row. The State
		// Manager has no denormalized namespaces to cascade (memberships reference
		// the organization by UUID).
		if req.Name != nil && *req.Name != org.Name {
			newName := *req.Name
			existing, err := h.orgRepo.GetByName(c.Request.Context(), newName)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check name availability"})
				return
			}
			if existing != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "Organization name already taken"})
				return
			}
			if err := h.orgRepo.Rename(c.Request.Context(), orgID, newName); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rename organization"})
				return
			}
			org.Name = newName
		}

		if req.DisplayName != nil {
			org.DisplayName = *req.DisplayName
		}

		// Update IdP binding — explicit empty clears, present value sets.
		if req.IdpType != nil {
			if *req.IdpType == "" {
				org.IdpType = nil
				org.IdpName = nil
			} else {
				valid := map[string]bool{"oidc": true, "saml": true, "ldap": true}
				if !valid[*req.IdpType] {
					c.JSON(http.StatusBadRequest, gin.H{"error": "idp_type must be 'oidc', 'saml', 'ldap', or empty to clear"})
					return
				}
				org.IdpType = req.IdpType
				org.IdpName = req.IdpName
			}
		}

		if err := h.orgRepo.Update(c.Request.Context(), org); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update organization"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"organization": org})
	}
}

// DeleteOrganizationHandler deletes an organization by ID.
// @Summary      Delete organization
// @Description  Remove an organization and its associated records.
// @Tags         Organizations
// @Produce      json
// @Param        id  path  string  true  "Organization ID"
// @Success      200  {object}  admin.MessageResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations/{id} [delete]
func (h *OrganizationHandlers) DeleteOrganizationHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")

		org, err := h.orgRepo.GetByID(c.Request.Context(), orgID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Organization not found"})
			return
		}

		if err := h.orgRepo.Delete(c.Request.Context(), orgID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete organization"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Organization deleted successfully"})
	}
}

// AddMemberRequest represents the request to add a member to an organization.
type AddMemberRequest struct {
	UserID         string  `json:"user_id" binding:"required"`
	RoleTemplateID *string `json:"role_template_id"` // Optional, UUID of role template
}

// AddMemberHandler adds a member to an organization.
// @Summary      Add organization member
// @Description  Add a user as a member to an organization, optionally assigning a role template.
// @Tags         Organizations
// @Accept       json
// @Produce      json
// @Param        id    path  string            true  "Organization ID"
// @Param        body  body  AddMemberRequest  true  "Member user_id and optional role_template_id"
// @Success      201  {object}  admin.MemberResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations/{id}/members [post]
func (h *OrganizationHandlers) AddMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")

		var req AddMemberRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
			return
		}

		org, err := h.orgRepo.GetByID(c.Request.Context(), orgID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve organization"})
			return
		}
		if org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Organization not found"})
			return
		}

		existingMember, err := h.orgRepo.GetMember(c.Request.Context(), orgID, req.UserID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check existing membership"})
			return
		}
		if existingMember != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "User is already a member of this organization"})
			return
		}

		member := &models.OrganizationMember{
			OrganizationID: orgID,
			UserID:         req.UserID,
			RoleTemplateID: req.RoleTemplateID,
			CreatedAt:      time.Now(),
		}
		if err := h.orgRepo.AddMember(c.Request.Context(), member); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add member to organization"})
			return
		}

		memberWithRole, err := h.orgRepo.GetMemberWithRole(c.Request.Context(), orgID, req.UserID)
		if err != nil {
			c.JSON(http.StatusCreated, gin.H{"member": member})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"member": memberWithRole})
	}
}

// UpdateMemberRequest represents the request to update a member's role template.
type UpdateMemberRequest struct {
	RoleTemplateID *string `json:"role_template_id"` // UUID of role template, or null to clear
}

// UpdateMemberHandler updates a member's role template in an organization.
// @Summary      Update organization member
// @Description  Update a member's role template within an organization.
// @Tags         Organizations
// @Accept       json
// @Produce      json
// @Param        id       path  string               true  "Organization ID"
// @Param        user_id  path  string               true  "User ID"
// @Param        body     body  UpdateMemberRequest  true  "role_template_id (UUID or null to clear)"
// @Success      200  {object}  admin.MemberResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations/{id}/members/{user_id} [put]
func (h *OrganizationHandlers) UpdateMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")
		userID := c.Param("user_id")

		var req UpdateMemberRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
			return
		}

		member, err := h.orgRepo.GetMember(c.Request.Context(), orgID, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve member"})
			return
		}
		if member == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Member not found in organization"})
			return
		}

		member.RoleTemplateID = req.RoleTemplateID
		if err := h.orgRepo.UpdateMember(c.Request.Context(), member); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update member role"})
			return
		}

		memberWithRole, err := h.orgRepo.GetMemberWithRole(c.Request.Context(), orgID, userID)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"member": member})
			return
		}
		c.JSON(http.StatusOK, gin.H{"member": memberWithRole})
	}
}

// RemoveMemberHandler removes a member from an organization.
// @Summary      Remove organization member
// @Description  Remove a user from an organization's membership.
// @Tags         Organizations
// @Produce      json
// @Param        id       path  string  true  "Organization ID"
// @Param        user_id  path  string  true  "User ID"
// @Success      200  {object}  admin.MessageResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations/{id}/members/{user_id} [delete]
func (h *OrganizationHandlers) RemoveMemberHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := c.Param("id")
		userID := c.Param("user_id")

		if err := h.orgRepo.RemoveMember(c.Request.Context(), orgID, userID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to remove member from organization"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Member removed successfully"})
	}
}

// SearchOrganizationsHandler searches organizations by name or display name.
// @Summary      Search organizations
// @Description  Search organizations by name or display name with pagination.
// @Tags         Organizations
// @Produce      json
// @Param        q         query  string  true   "Search query"
// @Param        page      query  int     false  "Page number (default 1)"
// @Param        per_page  query  int     false  "Items per page, max 100 (default 20)"
// @Success      200  {object}  admin.ListOrganizationsResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /organizations/search [get]
func (h *OrganizationHandlers) SearchOrganizationsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		query := c.Query("q")
		if query == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Search query is required"})
			return
		}

		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "20"))
		if page < 1 {
			page = 1
		}
		if perPage < 1 || perPage > 100 {
			perPage = 20
		}
		offset := (page - 1) * perPage

		orgs, err := h.orgRepo.Search(c.Request.Context(), query, perPage, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to search organizations"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"organizations": orgs,
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
			},
		})
	}
}
