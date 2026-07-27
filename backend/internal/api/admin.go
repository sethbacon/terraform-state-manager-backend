package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// AdminHandlers serves the identity-management read views (users, organizations,
// roles, audit log) backed by the shared terraform-suite-identity repositories.
// All routes are gated by the admin scope in the router.
type AdminHandlers struct {
	userRepo   *idstore.UserRepository
	orgRepo    *idstore.OrganizationRepository
	roleRepo   *idstore.RoleTemplateRepository
	auditRepo  *idstore.AuditRepository
	apiKeyRepo *idstore.APIKeyRepository
}

// NewAdminHandlers builds the admin handlers over the identity-schema connection.
func NewAdminHandlers(identityDB *sqlx.DB) *AdminHandlers {
	return &AdminHandlers{
		userRepo:   idstore.NewUserRepository(identityDB.DB),
		orgRepo:    idstore.NewOrganizationRepository(identityDB.DB),
		roleRepo:   idstore.NewRoleTemplateRepository(identityDB),
		auditRepo:  idstore.NewAuditRepository(identityDB.DB),
		apiKeyRepo: idstore.NewAPIKeyRepository(identityDB.DB),
	}
}

// revokeUserAPIKeys deletes every API key owned by userID. API keys carry static
// scopes validated only against the key row (the owner's live scopes are never
// re-derived), and have no natural expiry, so an offboarded or erased user's key
// keeps authenticating at its original — possibly admin — scope unless revoked
// here. Returns the number revoked so callers can record it in the audit trail.
func (h *AdminHandlers) revokeUserAPIKeys(ctx context.Context, userID string) (int, error) {
	keys, err := h.apiKeyRepo.ListAPIKeysByUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	for _, k := range keys {
		if err := h.apiKeyRepo.RevokeAPIKey(ctx, k.ID); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
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
		// Narrow the global user list to users who share an organization the caller
		// administers. The outer /admin gate accepts any single-org admin's flat
		// ScopeAdmin, so without this an admin of one org could enumerate every
		// tenant's users (#182).
		callerID, _ := c.Get("user_id")
		uid, _ := callerID.(string)
		adminOrgs, err := adminOrgSet(c.Request.Context(), h.orgRepo, uid)
		if err != nil {
			serverError(c, err, "failed to resolve caller organizations")
			return
		}
		limit, offset := pageParams(c)
		if q := c.Query("q"); q != "" {
			users, err := h.userRepo.SearchWithMemberships(c.Request.Context(), q, limit, offset)
			if err != nil {
				serverError(c, err, "failed to search users")
				return
			}
			users = usersInAdminOrgs(users, adminOrgs)
			// Search has no exact count; clients page until a short page comes back.
			c.JSON(http.StatusOK, gin.H{"users": users, "total": offset + len(users)})
			return
		}
		users, _, err := h.userRepo.ListUsersWithMemberships(c.Request.Context(), limit, offset)
		if err != nil {
			serverError(c, err, "failed to list users")
			return
		}
		users = usersInAdminOrgs(users, adminOrgs)
		// Post-filtered per page, so the count is page-relative (like the search
		// path) rather than the unfiltered DB total.
		c.JSON(http.StatusOK, gin.H{"users": users, "total": offset + len(users)})
	}
}

// usersInAdminOrgs keeps only the users the caller may see under #182: those
// sharing at least one organization with the caller's admin orgs, plus users
// with no memberships at all (no cross-tenant boundary to protect, mirroring
// requireSharedOrgAdminWithTargetUser).
func usersInAdminOrgs(users []*idmodels.UserWithOrgRoles, adminOrgs map[string]struct{}) []*idmodels.UserWithOrgRoles {
	out := make([]*idmodels.UserWithOrgRoles, 0, len(users))
	for _, u := range users {
		if len(u.Memberships) == 0 {
			out = append(out, u)
			continue
		}
		for _, m := range u.Memberships {
			if _, ok := adminOrgs[m.OrganizationID]; ok {
				out = append(out, u)
				break
			}
		}
	}
	return out
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
			serverError(c, err, "failed to list organizations")
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
			serverError(c, err, "failed to list role templates")
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

// auditLogJSON maps an identity AuditLog onto a snake_case JSON shape. The
// identity model carries json tags only on its transient join fields, so
// marshalling it directly would emit Go field names (ID, CreatedAt, …) that no
// client expects.
func auditLogJSON(l *idmodels.AuditLog) gin.H {
	return gin.H{
		"id":              l.ID,
		"user_id":         l.UserID,
		"organization_id": l.OrganizationID,
		"action":          l.Action,
		"resource_type":   l.ResourceType,
		"resource_id":     l.ResourceID,
		"metadata":        l.Metadata,
		"ip_address":      l.IPAddress,
		"created_at":      l.CreatedAt,
		"user_email":      l.UserEmail,
		"user_name":       l.UserName,
	}
}

func auditLogsJSON(logs []*idmodels.AuditLog) []gin.H {
	out := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		out = append(out, auditLogJSON(l))
	}
	return out
}

// auditLogsInAdminOrgs keeps only the audit entries the caller may see (#298):
// entries with no organization_id are platform-level (state/source actions on the
// single shared pool, login, startup) and stay visible to every admin; org-tagged
// entries (organization and member mutations) are shown only when the caller
// administers that organization. Mirrors usersInAdminOrgs.
func auditLogsInAdminOrgs(logs []*idmodels.AuditLog, adminOrgs map[string]struct{}) []*idmodels.AuditLog {
	out := make([]*idmodels.AuditLog, 0, len(logs))
	for _, l := range logs {
		if l.OrganizationID == nil || *l.OrganizationID == "" {
			out = append(out, l)
			continue
		}
		if _, ok := adminOrgs[*l.OrganizationID]; ok {
			out = append(out, l)
		}
	}
	return out
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
		// Narrow the audit trail to the caller's admin organizations (#298/#182).
		// The outer /admin gate accepts any single-org admin's flat ScopeAdmin, so
		// without this an admin of one org could read another tenant's
		// member-management trail. Org-tagged events (organization + member
		// mutations) are shown only to an admin of that org; genuinely org-less
		// platform events (login, and state/source actions — state_sources are a
		// single shared pool with no per-tenant owner) carry no organization_id and
		// remain visible to every admin.
		callerID, _ := c.Get("user_id")
		uid, _ := callerID.(string)
		adminOrgs, err := adminOrgSet(c.Request.Context(), h.orgRepo, uid)
		if err != nil {
			serverError(c, err, "failed to resolve caller organizations")
			return
		}
		limit, offset := pageParams(c)
		logs, _, err := h.auditRepo.ListAuditLogs(c.Request.Context(), auditFiltersFromQuery(c), limit, offset)
		if err != nil {
			serverError(c, err, "failed to list audit logs")
			return
		}
		logs = auditLogsInAdminOrgs(logs, adminOrgs)
		// Post-filtered per page, so the count is page-relative (like ListUsers)
		// rather than the unfiltered DB total.
		c.JSON(http.StatusOK, gin.H{"logs": auditLogsJSON(logs), "total": offset + len(logs)})
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
