package admin

import (
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// UserHandlers provides HTTP handlers for user management CRUD operations.
type UserHandlers struct {
	cfg      *config.Config
	db       *sql.DB
	userRepo *repositories.UserRepository
	orgRepo  *repositories.OrganizationRepository
}

// NewUserHandlers creates a new UserHandlers instance with the given config and database.
func NewUserHandlers(cfg *config.Config, db *sql.DB) *UserHandlers {
	return &UserHandlers{
		cfg:      cfg,
		db:       db,
		userRepo: repositories.NewUserRepository(db),
		orgRepo:  repositories.NewOrganizationRepository(db),
	}
}

// CreateUserRequest represents the request body for creating a new user.
type CreateUserRequest struct {
	Email   string  `json:"email" binding:"required,email"`
	Name    string  `json:"name" binding:"required"`
	OIDCSub *string `json:"oidc_sub"`
}

// UpdateUserRequest represents the request body for updating an existing user.
type UpdateUserRequest struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

// ListUsersHandler returns a handler that lists users with pagination.
// Query params: page (default 1), per_page (default 20, max 100).
// @Summary      List users
// @Tags         Users
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        organization_id  query  string  false  "Filter by organization ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /users [get]
func (h *UserHandlers) ListUsersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		page, perPage := parsePagination(c)
		offset := (page - 1) * perPage

		users, total, err := h.userRepo.ListUsers(c.Request.Context(), offset, perPage)
		if err != nil {
			slog.Error("failed to list users", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"users": users,
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
				"total":    total,
			},
		})
	}
}

// GetUserHandler returns a handler that retrieves a single user by ID,
// including their organization memberships.
// @Summary      Get user
// @Tags         Users
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /users/{id} [get]
func (h *UserHandlers) GetUserHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user id is required"})
			return
		}

		user, err := h.userRepo.GetUserByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get user", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}

		// Fetch the user's organization memberships.
		orgs, err := h.listUserOrganizations(c, id)
		if err != nil {
			slog.Error("failed to list user organizations", "user_id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list user organizations"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"user":          user,
			"organizations": orgs,
		})
	}
}

// CreateUserHandler returns a handler that creates a new user.
// @Summary      Create user
// @Tags         Users
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        body  body  CreateUserRequest  true  "Create user request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /users [post]
func (h *UserHandlers) CreateUserHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateUserRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// Check email uniqueness.
		existing, err := h.userRepo.GetUserByEmail(c.Request.Context(), req.Email)
		if err != nil {
			slog.Error("failed to check email uniqueness", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check email uniqueness"})
			return
		}
		if existing != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "a user with this email already exists"})
			return
		}

		// Check OIDC sub uniqueness if provided.
		if req.OIDCSub != nil && *req.OIDCSub != "" {
			existingSub, err := h.userRepo.GetUserByOIDCSub(c.Request.Context(), *req.OIDCSub)
			if err != nil {
				slog.Error("failed to check OIDC sub uniqueness", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check OIDC sub uniqueness"})
				return
			}
			if existingSub != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "a user with this OIDC subject already exists"})
				return
			}
		}

		user := &models.User{
			Email:    req.Email,
			Name:     req.Name,
			OIDCSub:  req.OIDCSub,
			IsActive: true,
		}

		if err := h.userRepo.CreateUser(c.Request.Context(), user); err != nil {
			slog.Error("failed to create user", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"user": user})
	}
}

// UpdateUserHandler returns a handler that updates an existing user by ID.
// @Summary      Update user
// @Tags         Users
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Accept       json
// @Produce      json
// @Param        id    path  string             true  "Resource ID"
// @Param        body  body  UpdateUserRequest  true  "Update user request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /users/{id} [put]
func (h *UserHandlers) UpdateUserHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user id is required"})
			return
		}

		var req UpdateUserRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		user, err := h.userRepo.GetUserByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get user for update", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}

		// Check email uniqueness if email is being changed.
		if req.Email != nil && *req.Email != user.Email {
			existing, err := h.userRepo.GetUserByEmail(c.Request.Context(), *req.Email)
			if err != nil {
				slog.Error("failed to check email uniqueness", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check email uniqueness"})
				return
			}
			if existing != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "a user with this email already exists"})
				return
			}
			user.Email = *req.Email
		}

		if req.Name != nil {
			user.Name = *req.Name
		}

		if err := h.userRepo.UpdateUser(c.Request.Context(), user); err != nil {
			slog.Error("failed to update user", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"user": user})
	}
}

// DeleteUserHandler returns a handler that deletes a user by ID.
// @Summary      Delete user
// @Tags         Users
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /users/{id} [delete]
func (h *UserHandlers) DeleteUserHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user id is required"})
			return
		}

		user, err := h.userRepo.GetUserByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get user for deletion", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}

		if err := h.userRepo.DeleteUser(c.Request.Context(), id); err != nil {
			slog.Error("failed to delete user", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete user"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "user deleted successfully"})
	}
}

// SearchUsersHandler returns a handler that searches users by query string.
// Query params: q (required), page (default 1), per_page (default 20, max 100).
// @Summary      Search users
// @Tags         Users
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        q  query  string  true  "Search query"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /users/search [get]
func (h *UserHandlers) SearchUsersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		query := c.Query("q")
		if query == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'q' is required"})
			return
		}

		page, perPage := parsePagination(c)

		users, err := h.userRepo.SearchUsers(c.Request.Context(), query, perPage)
		if err != nil {
			slog.Error("failed to search users", "query", query, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to search users"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"users": users,
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
				"total":    len(users),
			},
		})
	}
}

// GetCurrentUserMembershipsHandler returns a handler that retrieves the
// organization memberships for the currently authenticated user.
// @Summary      Get current user memberships
// @Tags         Users
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /users/me/memberships [get]
func (h *UserHandlers) GetCurrentUserMembershipsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := c.Get("user_id")
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not authenticated"})
			return
		}

		uid, ok := userID.(string)
		if !ok || uid == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user identity"})
			return
		}

		memberships, err := h.listUserMemberships(c, uid)
		if err != nil {
			slog.Error("failed to get current user memberships", "user_id", uid, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get memberships"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"memberships": memberships})
	}
}

// GetUserMembershipsHandler returns a handler that retrieves the organization
// memberships for a specific user by ID.
// @Summary      Get user memberships
// @Tags         Users
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /users/{id}/memberships [get]
func (h *UserHandlers) GetUserMembershipsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user id is required"})
			return
		}

		// Verify the user exists.
		user, err := h.userRepo.GetUserByID(c.Request.Context(), id)
		if err != nil {
			slog.Error("failed to get user for memberships", "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}

		memberships, err := h.listUserMemberships(c, id)
		if err != nil {
			slog.Error("failed to get user memberships", "user_id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get memberships"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"memberships": memberships})
	}
}

// listUserOrganizations queries organization memberships for a given user,
// returning organization details along with the user's role in each.
func (h *UserHandlers) listUserOrganizations(c *gin.Context, userID string) ([]gin.H, error) {
	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT o.id, o.name, o.display_name, o.description, o.is_active,
		        om.role_template_id, rt.name as role_template_name
		 FROM organization_members om
		 JOIN organizations o ON om.organization_id = o.id
		 LEFT JOIN role_templates rt ON om.role_template_id = rt.id
		 WHERE om.user_id = $1
		 ORDER BY o.name`, userID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var orgs []gin.H
	for rows.Next() {
		var (
			orgID            string
			orgName          string
			orgDisplayName   string
			orgDescription   *string
			orgIsActive      bool
			roleTemplateID   *string
			roleTemplateName *string
		)
		if err := rows.Scan(&orgID, &orgName, &orgDisplayName, &orgDescription, &orgIsActive,
			&roleTemplateID, &roleTemplateName); err != nil {
			return nil, err
		}
		orgs = append(orgs, gin.H{
			"id":                 orgID,
			"name":               orgName,
			"display_name":       orgDisplayName,
			"description":        orgDescription,
			"is_active":          orgIsActive,
			"role_template_id":   roleTemplateID,
			"role_template_name": roleTemplateName,
		})
	}
	if orgs == nil {
		orgs = []gin.H{}
	}
	return orgs, nil
}

// listUserMemberships queries all organization memberships for a given user,
// returning membership details including role information.
func (h *UserHandlers) listUserMemberships(c *gin.Context, userID string) ([]gin.H, error) {
	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT om.id, om.organization_id, o.name as organization_name, o.display_name as organization_display_name,
		        om.role_template_id, rt.name as role_template_name, om.created_at, om.updated_at
		 FROM organization_members om
		 JOIN organizations o ON om.organization_id = o.id
		 LEFT JOIN role_templates rt ON om.role_template_id = rt.id
		 WHERE om.user_id = $1
		 ORDER BY o.name`, userID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var memberships []gin.H
	for rows.Next() {
		var (
			membershipID            string
			organizationID          string
			organizationName        string
			organizationDisplayName string
			roleTemplateID          *string
			roleTemplateName        *string
			createdAt               interface{}
			updatedAt               interface{}
		)
		if err := rows.Scan(&membershipID, &organizationID, &organizationName, &organizationDisplayName,
			&roleTemplateID, &roleTemplateName, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		memberships = append(memberships, gin.H{
			"id":                        membershipID,
			"organization_id":           organizationID,
			"organization_name":         organizationName,
			"organization_display_name": organizationDisplayName,
			"role_template_id":          roleTemplateID,
			"role_template_name":        roleTemplateName,
			"created_at":                createdAt,
			"updated_at":                updatedAt,
		})
	}
	if memberships == nil {
		memberships = []gin.H{}
	}
	return memberships, nil
}

// parsePagination extracts page and per_page query parameters with defaults
// and bounds checking.
func parsePagination(c *gin.Context) (int, int) {
	page := 1
	perPage := 20

	if p := c.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}

	if pp := c.Query("per_page"); pp != "" {
		if v, err := strconv.Atoi(pp); err == nil && v > 0 {
			perPage = v
		}
	}

	if perPage > 100 {
		perPage = 100
	}

	return page, perPage
}
