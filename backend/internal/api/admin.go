package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// AdminHandlers serves the identity-management read views (users, organizations,
// roles, audit log) backed by the shared terraform-suite-identity repositories.
// All routes are gated by the admin scope in the router.
type AdminHandlers struct {
	userRepo  *idstore.UserRepository
	orgRepo   *idstore.OrganizationRepository
	roleRepo  *idstore.RoleTemplateRepository
	auditRepo *idstore.AuditRepository
}

// NewAdminHandlers builds the admin handlers over the identity-schema connection.
func NewAdminHandlers(identityDB *sqlx.DB) *AdminHandlers {
	return &AdminHandlers{
		userRepo:  idstore.NewUserRepository(identityDB.DB),
		orgRepo:   idstore.NewOrganizationRepository(identityDB.DB),
		roleRepo:  idstore.NewRoleTemplateRepository(identityDB),
		auditRepo: idstore.NewAuditRepository(identityDB.DB),
	}
}

func pageParams(c *gin.Context) (limit, offset int) {
	limit = 50
	if v, err := strconv.Atoi(c.Query("per_page")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	page := 1
	if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
		page = v
	}
	return limit, (page - 1) * limit
}

// ListUsers returns users with their organization memberships (paginated),
// optionally filtered by a free-text search on name/email (?q=).
// @Summary      List users
// @Description  Paginated list of users with organization role memberships. Requires admin.
// @Tags         Admin
// @Produce      json
// @Param        page      query  int     false  "Page (default 1)"
// @Param        per_page  query  int     false  "Items per page (max 200, default 50)"
// @Param        q         query  string  false  "Search by name or email"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/users [get]
func (h *AdminHandlers) ListUsers() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, offset := pageParams(c)
		if q := c.Query("q"); q != "" {
			users, err := h.userRepo.SearchWithMemberships(c.Request.Context(), q, limit, offset)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to search users"})
				return
			}
			// Search has no exact count; clients page until a short page comes back.
			c.JSON(http.StatusOK, gin.H{"users": users, "total": offset + len(users)})
			return
		}
		users, total, err := h.userRepo.ListUsersWithMemberships(c.Request.Context(), limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"users": users, "total": total})
	}
}

// ListOrganizations returns all organizations (paginated).
// @Summary      List organizations
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/organizations [get]
func (h *AdminHandlers) ListOrganizations() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, offset := pageParams(c)
		orgs, err := h.orgRepo.List(c.Request.Context(), limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list organizations"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"organizations": orgs})
	}
}

// ListRoles returns the role templates (the app owns these).
// @Summary      List role templates
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/roles [get]
func (h *AdminHandlers) ListRoles() gin.HandlerFunc {
	return func(c *gin.Context) {
		roles, err := h.roleRepo.ListRoleTemplates(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list role templates"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"roles": roles})
	}
}

// auditFiltersForUser builds the filter set selecting a single user's entries
// (used by the GDPR export).
func auditFiltersForUser(userID string) idstore.AuditFilters {
	return idstore.AuditFilters{UserID: &userID}
}

// auditFiltersFromQuery maps the audit-log query params onto repository filters.
func auditFiltersFromQuery(c *gin.Context) idstore.AuditFilters {
	var f idstore.AuditFilters
	if v := c.Query("action"); v != "" {
		f.Action = &v
	}
	if v := c.Query("resource_type"); v != "" {
		f.ResourceType = &v
	}
	if v := c.Query("user_email"); v != "" {
		f.UserEmail = &v
	}
	if v := c.Query("start_date"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.StartDate = &t
		}
	}
	if v := c.Query("end_date"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.EndDate = &t
		}
	}
	return f
}

// ListAuditLogs returns audit-log entries (paginated, filterable).
// @Summary      List audit log
// @Tags         Admin
// @Produce      json
// @Param        page           query  int     false  "Page (default 1)"
// @Param        per_page       query  int     false  "Items per page (max 200, default 50)"
// @Param        action         query  string  false  "Filter by action (partial)"
// @Param        resource_type  query  string  false  "Filter by resource type"
// @Param        user_email     query  string  false  "Filter by user email (partial)"
// @Param        start_date     query  string  false  "RFC3339 lower bound"
// @Param        end_date       query  string  false  "RFC3339 upper bound"
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/audit-logs [get]
func (h *AdminHandlers) ListAuditLogs() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, offset := pageParams(c)
		logs, total, err := h.auditRepo.ListAuditLogs(c.Request.Context(), auditFiltersFromQuery(c), limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list audit logs"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"logs": logs, "total": total})
	}
}

// Stats returns identity counts for the admin dashboard.
// @Summary      Admin stats
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/stats [get]
func (h *AdminHandlers) Stats() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		userCount, _ := h.userRepo.Count(ctx)
		orgs, _ := h.orgRepo.List(ctx, 1000, 0)
		roles, _ := h.roleRepo.ListRoleTemplates(ctx)
		c.JSON(http.StatusOK, gin.H{
			"users":         userCount,
			"organizations": len(orgs),
			"roles":         len(roles),
		})
	}
}
