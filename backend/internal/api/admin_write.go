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
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

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
		if err := h.userRepo.UpdateUser(c.Request.Context(), user); err != nil {
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
		// Revoke the user's API keys before removing the account so no key row is
		// orphaned and nothing can keep authenticating as the deleted user.
		revoked, err := h.revokeUserAPIKeys(ctx, id)
		if err != nil {
			serverError(c, err, "failed to revoke api keys")
			return
		}
		if err := h.userRepo.DeleteUser(ctx, id); err != nil {
			serverError(c, err, "failed to delete user")
			return
		}
		h.writeAudit(c, "user.delete", "user", id, map[string]interface{}{"api_keys_revoked": revoked})
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
		if err := h.userRepo.UpdateUser(ctx, user); err != nil {
			serverError(c, err, "failed to anonymize user")
			return
		}
		if err := h.orgRepo.RemoveAllMembershipsForUser(ctx, id); err != nil {
			serverError(c, err, "failed to revoke memberships")
			return
		}
		// GDPR erasure keeps the user row as a resolvable tombstone, so a
		// still-valid API key would keep authenticating at its stored scopes.
		// Revoke every key the user owns as part of the erasure.
		revoked, err := h.revokeUserAPIKeys(ctx, id)
		if err != nil {
			serverError(c, err, "failed to revoke api keys")
			return
		}
		h.writeAudit(c, "user.erase", "user", id, map[string]interface{}{"api_keys_revoked": revoked})
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
		id := c.Param("id")
		if err := h.orgRepo.Delete(c.Request.Context(), id); err != nil {
			serverError(c, err, "failed to delete organization")
			return
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
		if err := h.orgRepo.UpdateMemberRoleTemplate(c.Request.Context(), orgID, userID, roleID); err != nil {
			serverError(c, err, "failed to update member")
			return
		}
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
		if err := h.orgRepo.RemoveMember(c.Request.Context(), orgID, userID); err != nil {
			serverError(c, err, "failed to remove member")
			return
		}
		h.writeOrgAudit(c, "organization.member.remove", "organization", orgID, orgID, map[string]interface{}{"user_id": userID})
		c.Status(http.StatusNoContent)
	}
}
