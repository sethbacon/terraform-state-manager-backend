package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/credlifecycle"
)

// AdminHandlers serves the identity-management read views (users, organizations,
// roles, audit log) backed by the shared terraform-suite-identity repositories.
// All routes are gated by the admin scope in the router.
type AdminHandlers struct {
	userRepo *idstore.UserRepository
	// orgRepo is the shared organization repository WITH TSM's per-app role
	// mirror attached (internal/approles): every membership read here is the
	// library's, and every write is dual-written into this application's own
	// organization_member_roles. See approles.Members.
	orgRepo   *approles.Members
	roleRepo  *idstore.RoleTemplateRepository
	auditRepo *idstore.AuditRepository
	// creds invalidates the credential families that snapshot a principal's
	// derived authority whenever an admin action reduces it (#330). May be nil
	// (no sweep) so the handler set stays constructible without the revocation
	// subsystem.
	creds *credlifecycle.Sweeper
}

// AdminOption configures optional AdminHandlers construction behaviour.
type AdminOption func(*AdminHandlers)

// WithAdminCredentialSweeper wires the credential sweep the user- and
// membership-lifecycle routes perform.
func WithAdminCredentialSweeper(s *credlifecycle.Sweeper) AdminOption {
	return func(h *AdminHandlers) { h.creds = s }
}

// NewAdminHandlers builds the admin handlers over the identity-schema connection.
//
// appDB is the APPLICATION connection, and it is a required parameter rather
// than an option because it is what attaches the per-app role mirror. An option
// could be omitted, and an omitted mirror is a membership route that writes
// identity and nothing else — silently, and only on the deployment that forgot
// it. A nil appDB is legitimate ONLY in a rig with no application database; it
// yields a Members that performs the identity leg alone.
//
// It takes a *sql.DB, like every other handler constructor here: identity
// v0.25.0 made NewRoleTemplateRepository take one too, so the sqlx handle this
// signature used to demand — and the sqlx.NewDb wrapper every caller built to
// satisfy it — existed only to feed that one constructor.
func NewAdminHandlers(identityDB, appDB *sql.DB, opts ...AdminOption) *AdminHandlers {
	h := &AdminHandlers{
		userRepo:  idstore.NewUserRepository(identityDB),
		orgRepo:   approles.NewMembers(identityDB, appDB),
		roleRepo:  idstore.NewRoleTemplateRepository(identityDB),
		auditRepo: idstore.NewAuditRepository(identityDB),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// errCredentialSweepIncomplete reports that an offboarding sweep could not
// invalidate every credential family. The user-lifecycle routes treat it as
// fatal and answer 500 BEFORE the destructive step.
//
// identity v0.25.0's migration 000007 changed identity.api_keys.user_id from
// ON DELETE SET NULL to ON DELETE CASCADE, so a deleted user's key rows no
// longer outlive the owner as unattributable organization credentials. That is
// a BACKSTOP, not a reason to relax this: CASCADE runs after the fact and
// cannot reach a JWT whose scopes were embedded at login, so the sweep still
// has to succeed first and this error still has to be fatal.
var errCredentialSweepIncomplete = errors.New("credential sweep incomplete")

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
		// GUARD org-scope-user-list (#182): narrow the global user list to users
		// who share an organization the caller administers, plus users who belong
		// to no organization at all. The outer /admin gate accepts any single-org
		// admin's flat ScopeAdmin, so without this an admin of one org could
		// enumerate every tenant's users.
		//
		// Since identity v0.25.0 that narrowing is the query's own predicate (an
		// EXISTS over organization_members) rather than the usersInAdminOrgs
		// post-filter this handler used to apply. The predicate also bounds the
		// MEMBERSHIPS attached to each returned user, which the post-filter never
		// could: it could only drop a whole user, so a user the caller was
		// entitled to see arrived carrying their memberships in every other
		// tenant (#161).
		scope, err := h.callerOrgScope(c)
		if err != nil {
			serverError(c, err, "failed to resolve caller organizations")
			return
		}
		limit, offset := pageParams(c)
		if q := c.Query("q"); q != "" {
			users, err := h.userRepo.SearchWithMemberships(c.Request.Context(), q, limit, offset, scope)
			if err != nil {
				serverError(c, err, "failed to search users")
				return
			}
			// Search has no exact count; clients page until a short page comes back.
			c.JSON(http.StatusOK, gin.H{"users": users, "total": offset + len(users)})
			return
		}
		users, total, err := h.userRepo.ListUsersWithMemberships(c.Request.Context(), limit, offset, scope)
		if err != nil {
			serverError(c, err, "failed to list users")
			return
		}
		// total is now the scoped count straight from the repository: with the
		// tenant predicate pushed into SQL there is no post-filter to make it
		// disagree with the rows returned, so it no longer has to be reported
		// page-relative.
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
		// GUARD org-scope-org-list (identity #161): the route is gated on the flat
		// organizations:read scope, which names no organization, so it is not a
		// tenant boundary — exactly the shape that let this axis hand every
		// tenant's organization directory to any single-org member. Scoped to the
		// organizations the caller's role template grants organizations:read in,
		// which is the same set requireOrgScope re-derives for the :id routes.
		scope, err := h.callerScopeFor(c, auth.ScopeOrganizationsRead)
		if err != nil {
			serverError(c, err, "failed to resolve caller organizations")
			return
		}
		limit, offset := pageParams(c)
		orgs, err := h.orgRepo.List(c.Request.Context(), limit, offset, scope)
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
		// GUARD audit-scope-list (#298/#182): narrow the audit trail to the
		// caller's admin organizations plus org-less platform events. The outer
		// /admin gate accepts any single-org admin's flat ScopeAdmin, so without
		// this an admin of one org could read another tenant's member-management
		// trail. The narrowing is a SQL predicate rather than a post-filter, so
		// the database never returns another tenant's rows in the first place.
		scope, err := h.callerOrgScope(c)
		if err != nil {
			serverError(c, err, "failed to resolve caller organizations")
			return
		}
		limit, offset := pageParams(c)
		logs, total, err := h.auditRepo.ListAuditLogs(c.Request.Context(), auditFiltersFromQuery(c), scope, limit, offset)
		if err != nil {
			serverError(c, err, "failed to list audit logs")
			return
		}
		// total is now the scoped count straight from the repository: with the
		// tenant predicate pushed into SQL there is no post-filter to make it
		// disagree with the rows returned.
		c.JSON(http.StatusOK, gin.H{"logs": auditLogsJSON(logs), "total": total})
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
		// Each tile counts exactly what its list route would show the same
		// caller: users under the admin scope ListUsers uses, organizations
		// under the organizations:read scope ListOrganizations uses. A tile that
		// counted rows the caller cannot open is a cross-tenant existence oracle,
		// which is the disclosure half of the same class the lists close.
		userScope, uErr := h.callerOrgScope(c)
		orgScope, oErr := h.callerScopeFor(c, auth.ScopeOrganizationsRead)
		if uErr != nil || oErr != nil {
			// The zero OrgScope denies, so the counts below already fail closed;
			// this only keeps the failure from being silent.
			slog.Warn("admin stats: failed to resolve caller organizations", "user_scope_error", uErr, "org_scope_error", oErr)
		}
		userCount, _ := h.userRepo.Count(ctx, userScope)
		orgs, _ := h.orgRepo.List(ctx, 1000, 0, orgScope)
		roles, _ := h.roleRepo.ListRoleTemplates(ctx)
		c.JSON(http.StatusOK, gin.H{
			"users":         userCount,
			"organizations": len(orgs),
			"roles":         len(roles),
		})
	}
}
