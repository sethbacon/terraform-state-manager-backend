// users.go implements handlers for user account CRUD operations including
// listing, creating, updating, and deleting users. Mirrors the registry's
// identity surface 1:1 on the shared canonical identity model.
package admin

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// UserHandlers handles user management endpoints.
type UserHandlers struct {
	cfg      *config.Config
	db       *sql.DB
	userRepo *repositories.UserRepository
	orgRepo  *repositories.OrganizationRepository
}

// NewUserHandlers creates a new UserHandlers instance.
func NewUserHandlers(cfg *config.Config, db *sql.DB) *UserHandlers {
	return &UserHandlers{
		cfg:      cfg,
		db:       db,
		userRepo: repositories.NewUserRepository(db),
		orgRepo:  repositories.NewOrganizationRepository(db),
	}
}

// ListUsersHandler lists all users with pagination (including their memberships).
// @Summary      List users
// @Description  Get a paginated list of all users with their organization role templates. Requires users:read scope.
// @Tags         Users
// @Accept       json
// @Produce      json
// @Param        page      query  int  false  "Page number (default 1)"
// @Param        per_page  query  int  false  "Items per page, max 100 (default 20)"
// @Success      200  {object}  admin.ListUsersResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /users [get]
func (h *UserHandlers) ListUsersHandler() gin.HandlerFunc {
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

		users, total, err := h.userRepo.ListUsersWithMemberships(c.Request.Context(), perPage, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list users"})
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

// GetUserHandler retrieves a specific user by ID with their organizations.
// @Summary      Get user
// @Description  Get a user by ID with their organization memberships. Requires users:read scope.
// @Tags         Users
// @Produce      json
// @Param        id  path  string  true  "User ID"
// @Success      200  {object}  admin.UserWithOrgsResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /users/{id} [get]
func (h *UserHandlers) GetUserHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("id")

		user, err := h.userRepo.GetUserByID(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}

		orgs, err := h.orgRepo.ListUserOrganizations(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve user organizations"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"user":          user,
			"organizations": orgs,
		})
	}
}

// CreateUserRequest represents the request to create a new user.
type CreateUserRequest struct {
	Email   string  `json:"email" binding:"required,email"`
	Name    string  `json:"name" binding:"required"`
	OIDCSub *string `json:"oidc_sub"`
}

// CreateUserHandler creates a new user (admin only; users are typically created via OIDC).
// @Summary      Create user
// @Description  Create a new user. Typically users are created via OIDC; this endpoint is for admin use. Requires users:write scope.
// @Tags         Users
// @Accept       json
// @Produce      json
// @Param        body  body  CreateUserRequest  true  "User creation request"
// @Success      201  {object}  admin.UserResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /users [post]
func (h *UserHandlers) CreateUserHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateUserRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
			return
		}

		existingUser, err := h.userRepo.GetUserByEmail(c.Request.Context(), req.Email)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check existing user"})
			return
		}
		if existingUser != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "User with this email already exists"})
			return
		}

		user := &models.User{
			Email:   req.Email,
			Name:    req.Name,
			OIDCSub: req.OIDCSub,
		}
		if err := h.userRepo.Create(c.Request.Context(), user); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user"})
			return
		}

		c.JSON(http.StatusCreated, gin.H{"user": user})
	}
}

// UpdateUserRequest represents the request to update a user.
type UpdateUserRequest struct {
	Name  *string `json:"name"`
	Email *string `json:"email,omitempty"`
}

// UpdateUserHandler updates a user's name or email.
// @Summary      Update user
// @Description  Update a user's name or email. Requires users:write scope.
// @Tags         Users
// @Accept       json
// @Produce      json
// @Param        id    path  string             true  "User ID"
// @Param        body  body  UpdateUserRequest  true  "User update request"
// @Success      200  {object}  admin.UserResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /users/{id} [put]
func (h *UserHandlers) UpdateUserHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("id")

		var req UpdateUserRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
			return
		}

		user, err := h.userRepo.GetUserByID(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}

		if req.Name != nil {
			user.Name = *req.Name
		}
		if req.Email != nil {
			existingUser, err := h.userRepo.GetUserByEmail(c.Request.Context(), *req.Email)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check email availability"})
				return
			}
			if existingUser != nil && existingUser.ID != userID {
				c.JSON(http.StatusConflict, gin.H{"error": "Email already in use by another user"})
				return
			}
			user.Email = *req.Email
		}

		if err := h.userRepo.Update(c.Request.Context(), user); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update user"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"user": user})
	}
}

// DeleteUserHandler deletes a user by ID.
// @Summary      Delete user
// @Description  Delete a user by ID. Cascading deletes will handle related records. Requires users:write scope.
// @Tags         Users
// @Produce      json
// @Param        id  path  string  true  "User ID"
// @Success      200  {object}  admin.MessageResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /users/{id} [delete]
func (h *UserHandlers) DeleteUserHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("id")

		user, err := h.userRepo.GetUserByID(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}

		if err := h.userRepo.Delete(c.Request.Context(), userID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete user"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "User deleted successfully"})
	}
}

// SearchUsersHandler searches users by email or name.
// @Summary      Search users
// @Description  Search users by email or name. Requires users:read scope.
// @Tags         Users
// @Produce      json
// @Param        q         query  string  true   "Search query"
// @Param        page      query  int     false  "Page number (default 1)"
// @Param        per_page  query  int     false  "Items per page, max 100 (default 20)"
// @Success      200  {object}  admin.ListUsersResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /users/search [get]
func (h *UserHandlers) SearchUsersHandler() gin.HandlerFunc {
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

		users, err := h.userRepo.SearchWithMemberships(c.Request.Context(), query, perPage, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to search users"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"users": users,
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
			},
		})
	}
}

// GetCurrentUserMembershipsHandler retrieves the current user's organization memberships.
// @Summary      Get current user memberships
// @Description  Get the organization memberships for the currently authenticated user. No special scopes required.
// @Tags         Users
// @Produce      json
// @Success      200  {object}  admin.UserMembershipsResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /users/me/memberships [get]
func (h *UserHandlers) GetCurrentUserMembershipsHandler() gin.HandlerFunc {
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

		memberships, err := h.orgRepo.GetUserMemberships(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve user memberships"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"memberships": memberships})
	}
}

// GetUserMembershipsHandler retrieves a specific user's organization memberships.
// @Summary      Get user memberships
// @Description  Get the organization memberships for a specific user. Requires users:read scope.
// @Tags         Users
// @Produce      json
// @Param        id  path  string  true  "User ID"
// @Success      200  {object}  admin.UserMembershipsResponse
// @Failure      401  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /users/{id}/memberships [get]
func (h *UserHandlers) GetUserMembershipsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.Param("id")

		user, err := h.userRepo.GetUserByID(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve user"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
			return
		}

		memberships, err := h.orgRepo.GetUserMemberships(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve user memberships"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"memberships": memberships})
	}
}
