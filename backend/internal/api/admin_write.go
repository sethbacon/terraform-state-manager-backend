// admin_write.go implements the identity-management write endpoints (user CRUD +
// GDPR data-subject actions, organization CRUD, organization member management)
// that back the admin pages, mirroring the registry's identity API surface.
// Every mutation writes an audit-log entry. User routes remain gated on the
// flat/global admin scope; organization routes require organization-tier
// scopes instead (organizations:read/:write/:create — see admin_org_scope.go
// and router.go).
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// notFound reports whether err is the identity store's not-found sentinel.
//
// Since terraform-suite-identity v0.24.0 a by-id read that misses, and a by-id
// UPDATE/DELETE that matches zero rows, both report an error wrapping
// store.ErrNotFound instead of succeeding silently. Every handler below that
// used to lean on "nil error means it happened" therefore has to say which of
// the two answers it wants: 404 (the resource is gone, and the caller asked
// about a specific one) or "already in the desired state" (idempotent DELETE).
// One helper so the choice is visible at each site rather than re-derived.
func notFound(err error) bool { return errors.Is(err, idstore.ErrNotFound) }

// buildAuditLog assembles an AuditLog from the request context (acting user,
// client IP) and the given fields. A non-empty orgID stamps the owning
// organization so the admin audit view can be narrowed to that tenant (#298); an
// empty orgID leaves the entry org-less (a platform-level event).
func buildAuditLog(c *gin.Context, action, resourceType, resourceID, orgID string, metadata map[string]interface{}) *idmodels.AuditLog {
	entry := &idmodels.AuditLog{
		Action:       action,
		ResourceType: &resourceType,
		Metadata:     metadata,
	}
	if resourceID != "" {
		entry.ResourceID = &resourceID
	}
	if orgID != "" {
		entry.OrganizationID = &orgID
	}
	if v, ok := c.Get("user_id"); ok {
		if uid, ok := v.(string); ok && uid != "" {
			entry.UserID = &uid
		}
	}
	if ip := c.ClientIP(); ip != "" {
		entry.IPAddress = &ip
	}
	return entry
}

// writeAuditEntry records a platform-level mutation (no owning organization) in
// the audit log with the acting user and client IP (best-effort — an audit write
// failure is logged, never blocks the mutation response). Shared by the admin, CI
// source, and pipeline handlers.
func writeAuditEntry(c *gin.Context, repo *idstore.AuditRepository, action, resourceType, resourceID string, metadata map[string]interface{}) {
	writeAuditEntryOrg(c, repo, action, resourceType, resourceID, "", metadata)
}

// writeAuditEntryOrg is writeAuditEntry that also attributes the entry to an
// organization (#298), so an audit event for a specific tenant is shown only to
// that tenant's admins by ListAuditLogs.
func writeAuditEntryOrg(c *gin.Context, repo *idstore.AuditRepository, action, resourceType, resourceID, orgID string, metadata map[string]interface{}) {
	entry := buildAuditLog(c, action, resourceType, resourceID, orgID, metadata)
	if err := repo.CreateAuditLog(c.Request.Context(), entry); err != nil {
		slog.Warn("failed to write audit entry", "action", action, "error", err)
	}
}

func (h *AdminHandlers) writeAudit(c *gin.Context, action, resourceType, resourceID string, metadata map[string]interface{}) {
	writeAuditEntry(c, h.auditRepo, action, resourceType, resourceID, metadata)
}

// writeOrgAudit records a mutation scoped to a known organization, stamping the
// audit entry with orgID so it is visible only to that org's admins (#298).
func (h *AdminHandlers) writeOrgAudit(c *gin.Context, action, resourceType, resourceID, orgID string, metadata map[string]interface{}) {
	writeAuditEntryOrg(c, h.auditRepo, action, resourceType, resourceID, orgID, metadata)
}

// --- Users ---

type userRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// CreateUser creates a user.
// @Summary      Create user
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/users [post]
func (h *AdminHandlers) CreateUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req userRequest
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Email) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "email is required"})
			return
		}
		user := &idmodels.User{Email: strings.TrimSpace(req.Email), Name: strings.TrimSpace(req.Name)}
		if err := h.userRepo.CreateUser(c.Request.Context(), user); err != nil {
			serverError(c, err, "failed to create user")
			return
		}
		h.writeAudit(c, "user.create", "user", user.ID, map[string]interface{}{"email": user.Email})
		c.JSON(http.StatusCreated, user)
	}
}

// UpdateUser updates a user's display name.
// @Summary      Update user
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/users/{id} [put]
func (h *AdminHandlers) UpdateUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req userRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		user, err := h.userRepo.GetUserByID(c.Request.Context(), c.Param("id"))
		if err != nil || user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		if strings.TrimSpace(req.Name) != "" {
			user.Name = strings.TrimSpace(req.Name)
		}
		// The row was read a moment ago, so a zero-row UPDATE means it was
		// deleted in between. Answer the same 404 the read above would have —
		// not a 500, and not the silent 200 this returned before v0.24.0 made
		// the zero-row write distinguishable.
		if err := h.userRepo.UpdateUser(c.Request.Context(), user); err != nil {
			if notFound(err) {
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
				return
			}
			serverError(c, err, "failed to update user")
			return
		}
		h.writeAudit(c, "user.update", "user", user.ID, nil)
		c.JSON(http.StatusOK, user)
	}
}

// DeleteUser deletes a user (memberships cascade).
// @Summary      Delete user
// @Tags         Admin
// @Produce      json
// @Success      204
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/users/{id} [delete]
func (h *AdminHandlers) DeleteUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		id := c.Param("id")
		// Invalidate every credential family before removing the account: the
		// API-key rows (which outlive the owner — user_id is ON DELETE SET NULL —
		// and would otherwise keep authenticating at their frozen scopes) and the
		// user's live JWT sessions, whose scopes were embedded at login and which
		// the JTI denylist cannot reach because no JTI is recorded (#330).
		out := h.creds.UserDeprovisioned(ctx, id, "admin: user deleted")
		if out.Incomplete {
			serverError(c, errCredentialSweepIncomplete, "failed to revoke credentials")
			return
		}
		// DELETE is idempotent here and stays that way: this route has never
		// pre-checked existence, so deleting an already-absent user answered 204
		// before v0.24.0 and must keep answering 204. ErrNotFound is exactly
		// that case ("nothing to delete"), so it is absorbed rather than turned
		// into the 500 a bare `err != nil` would now produce on every repeat
		// call. A real failure still 500s.
		if err := h.userRepo.DeleteUser(ctx, id); err != nil && !notFound(err) {
			serverError(c, err, "failed to delete user")
			return
		}
		h.writeAudit(c, "user.delete", "user", id, map[string]interface{}{
			"api_keys_revoked": out.KeysRevoked, "sessions_revoked": out.TokensRevoked})
		c.Status(http.StatusNoContent)
	}
}

// GetUserMemberships returns a user's organization memberships.
// @Summary      Get user memberships
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/users/{id}/memberships [get]
func (h *AdminHandlers) GetUserMemberships() gin.HandlerFunc {
	return func(c *gin.Context) {
		memberships, err := h.orgRepo.GetUserMemberships(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to load memberships")
			return
		}
		c.JSON(http.StatusOK, gin.H{"memberships": memberships})
	}
}

// --- GDPR data-subject endpoints ---

// ExportUserData exports all personal data associated with a user as a JSON
// download (GDPR Articles 15/20).
// @Summary      Export user data (GDPR)
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/users/{id}/export [get]
func (h *AdminHandlers) ExportUserData() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		id := c.Param("id")
		user, err := h.userRepo.GetUserByID(ctx, id)
		if err != nil || user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		memberships, err := h.orgRepo.GetUserMemberships(ctx, id)
		if err != nil {
			serverError(c, err, "failed to load memberships")
			return
		}
		// Audit entries attributed to the user (capped — exports are not a
		// general audit-archival mechanism).
		//
		// GUARD audit-scope-user-export (#331): scoped to the CALLER, not the
		// target. requireSharedOrgAdminWithTargetUser only proves the caller
		// administers at least ONE organization the target belongs to, so an
		// unscoped read would hand the caller the target's trail from every
		// OTHER tenant the target also belongs to — the same leak as the bulk
		// export, reached through the per-user axis.
		scope, err := h.callerAuditScope(c)
		if err != nil {
			serverError(c, err, "failed to resolve caller organizations")
			return
		}
		logs, _, err := h.auditRepo.ListAuditLogs(ctx, auditFiltersForUser(id), scope, 1000, 0)
		if err != nil {
			serverError(c, err, "failed to load audit entries")
			return
		}
		export := gin.H{
			"exported_at": time.Now().UTC().Format(time.RFC3339),
			"user":        user,
			"memberships": memberships,
			"audit_logs":  auditLogsJSON(logs),
		}
		data, err := json.MarshalIndent(export, "", "  ")
		if err != nil {
			serverError(c, err, "failed to encode export")
			return
		}
		h.writeAudit(c, "user.export", "user", id, nil)
		c.Header("Content-Disposition", "attachment; filename=user-data-"+id+".json")
		c.Data(http.StatusOK, "application/json", data)
	}
}

// EraseUser anonymizes a user's PII and revokes all access (GDPR Article 17).
// The user row is kept as a tombstone so audit entries stay attributable to an
// anonymized identifier; oidc_sub is cleared so an IdP re-login can never
// resurrect the erased account.
// @Summary      Erase user data (GDPR)
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/users/{id}/erase [post]
func (h *AdminHandlers) EraseUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		id := c.Param("id")
		user, err := h.userRepo.GetUserByID(ctx, id)
		if err != nil || user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		user.Email = "erased-" + id + "@anonymized.invalid"
		user.Name = "Erased User"
		user.OIDCSub = nil
		// Raced delete between the read above and this write: 404, matching the
		// read's own answer. Anonymization reporting success when it wrote no
		// row is the exact GDPR-relevant lie v0.24.0 makes detectable.
		if err := h.userRepo.UpdateUser(ctx, user); err != nil {
			if notFound(err) {
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
				return
			}
			serverError(c, err, "failed to anonymize user")
			return
		}
		// The bulk sweep reports an affected-row count rather than ErrNotFound:
		// a user who holds no memberships is already in the erased end state.
		if _, err := h.orgRepo.RemoveAllMembershipsForUser(ctx, id); err != nil {
			serverError(c, err, "failed to revoke memberships")
			return
		}
		// GDPR erasure keeps the user row as a resolvable tombstone, so both
		// credential families survive it: a still-valid API key would keep
		// authenticating at its stored scopes, and a live session would keep
		// serving the scope union the memberships just removed (#330).
		out := h.creds.UserDeprovisioned(ctx, id, "admin: user erased (GDPR)")
		if out.Incomplete {
			serverError(c, errCredentialSweepIncomplete, "failed to revoke credentials")
			return
		}
		h.writeAudit(c, "user.erase", "user", id, map[string]interface{}{
			"api_keys_revoked": out.KeysRevoked, "sessions_revoked": out.TokensRevoked})
		c.JSON(http.StatusOK, gin.H{
			"message": "User data has been erased. Audit log entries are preserved with anonymized identifiers.",
			"user_id": id,
		})
	}
}

// --- Organizations ---

type orgRequest struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	IdpType     *string `json:"idp_type"`
	IdpName     *string `json:"idp_name"`
}

// CreateOrganization creates an organization.
// @Summary      Create organization
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/organizations [post]
func (h *AdminHandlers) CreateOrganization() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req orgRequest
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		org := &idmodels.Organization{
			Name:        strings.TrimSpace(req.Name),
			DisplayName: strings.TrimSpace(req.DisplayName),
		}
		ctx := c.Request.Context()
		if err := h.orgRepo.CreateOrganization(ctx, org); err != nil {
			serverError(c, err, "failed to create organization")
			return
		}
		// Auto-add the caller as the new organization's org_owner member. Every
		// other /admin/organizations/:id* route (including member management)
		// requires the caller to hold organizations:write (or admin) within that
		// exact organization (requireOrgScope, see admin_org_scope.go) — without
		// this, a brand-new organization would have no member able to manage it
		// at all, since nobody could pass that check for an org with zero
		// members. org_owner carries organizations:write, so the creator can
		// still fully administer the organization they just created.
		if callerID, ok := c.Get("user_id"); ok {
			if uid, _ := callerID.(string); uid != "" {
				if err := h.orgRepo.AddMemberWithParams(ctx, org.ID, uid, "org_owner"); err != nil {
					serverError(c, err, "organization created, but failed to add you as its owner")
					return
				}
			}
		}
		h.writeOrgAudit(c, "organization.create", "organization", org.ID, org.ID, map[string]interface{}{"name": org.Name})
		c.JSON(http.StatusCreated, org)
	}
}

// UpdateOrganization updates display name / IdP binding, and renames when a new
// name is supplied.
// @Summary      Update organization
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/organizations/{id} [put]
func (h *AdminHandlers) UpdateOrganization() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		var req orgRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		org, err := h.orgRepo.GetByID(ctx, c.Param("id"))
		if err != nil || org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}
		if name := strings.TrimSpace(req.Name); name != "" && name != org.Name {
			if err := h.orgRepo.Rename(ctx, org.ID, name); err != nil {
				if notFound(err) {
					c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
					return
				}
				serverError(c, err, "failed to rename organization")
				return
			}
			org.Name = name
		}
		if strings.TrimSpace(req.DisplayName) != "" {
			org.DisplayName = strings.TrimSpace(req.DisplayName)
		}
		// IdP binding: empty string clears, non-empty sets, absent (nil) keeps.
		if req.IdpType != nil {
			org.IdpType = nilIfEmpty(*req.IdpType)
		}
		if req.IdpName != nil {
			org.IdpName = nilIfEmpty(*req.IdpName)
		}
		if err := h.orgRepo.Update(ctx, org); err != nil {
			if notFound(err) {
				c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
				return
			}
			serverError(c, err, "failed to update organization")
			return
		}
		h.writeOrgAudit(c, "organization.update", "organization", org.ID, org.ID, nil)
		c.JSON(http.StatusOK, org)
	}
}

func nilIfEmpty(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// DeleteOrganization removes an organization.
// @Summary      Delete organization
// @Tags         Admin
// @Produce      json
// @Success      204
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/organizations/{id} [delete]
func (h *AdminHandlers) DeleteOrganization() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		id := c.Param("id")
		// organization_members.organization_id is ON DELETE CASCADE, so dropping
		// the organization silently reduces every member's authority — a
		// reduction with no membership statement of its own. Snapshot the members
		// FIRST: after the delete there is nobody left to sweep (#330).
		members, err := h.orgRepo.ListMembers(ctx, id)
		if err != nil {
			serverError(c, err, "failed to list organization members")
			return
		}
		// Idempotent DELETE (see DeleteUser): an already-absent organization
		// answered 204 before v0.24.0 and keeps answering 204.
		if err := h.orgRepo.Delete(ctx, id); err != nil && !notFound(err) {
			serverError(c, err, "failed to delete organization")
			return
		}
		for _, m := range members {
			h.creds.AuthorityReduced(ctx, m.UserID, "admin: organization deleted")
		}
		h.writeOrgAudit(c, "organization.delete", "organization", id, id, nil)
		c.Status(http.StatusNoContent)
	}
}

// ListOrganizationMembers returns an organization's members with user details.
// @Summary      List organization members
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/organizations/{id}/members [get]
func (h *AdminHandlers) ListOrganizationMembers() gin.HandlerFunc {
	return func(c *gin.Context) {
		members, err := h.orgRepo.ListMembersWithUsers(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to list members")
			return
		}
		c.JSON(http.StatusOK, gin.H{"members": members})
	}
}

type memberRequest struct {
	UserID         string  `json:"user_id"`
	RoleTemplateID *string `json:"role_template_id"`
}

// validRoleTemplateID parses an optional role template id, returning nil for
// absent/empty values and an error for malformed ones.
func validRoleTemplateID(raw *string) (*string, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, true
	}
	id := strings.TrimSpace(*raw)
	if _, err := uuid.Parse(id); err != nil {
		return nil, false
	}
	return &id, true
}

// AddOrganizationMember adds a user to an organization with an optional role.
// @Summary      Add organization member
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/organizations/{id}/members [post]
func (h *AdminHandlers) AddOrganizationMember() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req memberRequest
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.UserID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required"})
			return
		}
		if chk := h.checkRoleAssignment(c, req.RoleTemplateID); !chk.allowed {
			c.JSON(chk.status, gin.H{"error": "role assignment not permitted"})
			return
		}
		roleID, ok := validRoleTemplateID(req.RoleTemplateID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role_template_id"})
			return
		}
		orgID := c.Param("id")
		if err := h.orgRepo.AddMemberWithRoleTemplate(c.Request.Context(), orgID, req.UserID, roleID); err != nil {
			serverError(c, err, "failed to add member")
			return
		}
		h.writeOrgAudit(c, "organization.member.add", "organization", orgID, orgID, map[string]interface{}{"user_id": req.UserID})
		c.JSON(http.StatusCreated, gin.H{"status": "added"})
	}
}

// UpdateOrganizationMember changes a member's role template.
// @Summary      Update organization member role
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/organizations/{id}/members/{user_id} [put]
func (h *AdminHandlers) UpdateOrganizationMember() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req memberRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		if chk := h.checkRoleAssignment(c, req.RoleTemplateID); !chk.allowed {
			c.JSON(chk.status, gin.H{"error": "role assignment not permitted"})
			return
		}
		roleID, ok := validRoleTemplateID(req.RoleTemplateID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role_template_id"})
			return
		}
		orgID, userID := c.Param("id"), c.Param("user_id")
		ctx := c.Request.Context()
		// Absorbed, not 404'd: this route has never pre-checked membership, so a
		// PUT naming a non-member answered 200 before v0.24.0 and keeps
		// answering 200 (rule: no silent status-code changes in this bump).
		// The AuthorityReduced sweep below is safe either way — it re-derives
		// the user's retained scopes rather than assuming a change happened.
		// Whether this endpoint SHOULD 404 on a non-member is a deliberate API
		// decision, tracked separately from the identity upgrade.
		if err := h.orgRepo.UpdateMemberRoleTemplate(ctx, orgID, userID, roleID); err != nil && !notFound(err) {
			serverError(c, err, "failed to update member")
			return
		}
		// A reassignment can narrow what the member holds, and both credential
		// families froze the previous scope set. Run after the write so the
		// retained authority is re-derived from the new role template (#330).
		h.creds.AuthorityReduced(ctx, userID, "admin: organization member role changed")
		h.writeOrgAudit(c, "organization.member.update", "organization", orgID, orgID, map[string]interface{}{"user_id": userID})
		c.JSON(http.StatusOK, gin.H{"status": "updated"})
	}
}

// RemoveOrganizationMember removes a user from an organization.
// @Summary      Remove organization member
// @Tags         Admin
// @Produce      json
// @Success      204
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/organizations/{id}/members/{user_id} [delete]
func (h *AdminHandlers) RemoveOrganizationMember() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID, userID := c.Param("id"), c.Param("user_id")
		ctx := c.Request.Context()
		// Idempotent DELETE (see DeleteUser): removing a user who is not a
		// member answered 204 before v0.24.0 and keeps answering 204. The sweep
		// below still runs, which is the safe direction — it recomputes the
		// authority the user retains rather than trusting that a row moved.
		if err := h.orgRepo.RemoveMember(ctx, orgID, userID); err != nil && !notFound(err) {
			serverError(c, err, "failed to remove member")
			return
		}
		// The membership is gone; the credentials minted while it existed are
		// not. Sweep both families against the authority the user retains in
		// their remaining organizations (#330).
		h.creds.AuthorityReduced(ctx, userID, "admin: organization membership removed")
		h.writeOrgAudit(c, "organization.member.remove", "organization", orgID, orgID, map[string]interface{}{"user_id": userID})
		c.Status(http.StatusNoContent)
	}
}
