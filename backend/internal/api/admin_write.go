// admin_write.go implements the identity-management write endpoints (user CRUD +
// GDPR data-subject actions, organization CRUD, organization member management)
// that back the admin pages, mirroring the registry's identity API surface.
// Every mutation writes an audit-log entry. User routes remain gated on the
// flat/global admin scope; organization routes require organization-tier
// scopes instead (organizations:read/:write/:create — see admin_org_scope.go
// and router.go).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idmodels "github.com/sethbacon/terraform-suite-identity/identity/models"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/approles"
	"github.com/terraform-state-manager/terraform-state-manager/internal/credlifecycle"
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

// sweepReduced adapts this app's credential sweeper to approles.AuthorityReducer,
// so an authority-reducing role write cannot be spelled without the sweep that
// retires the credentials which froze that authority (#330).
//
// The Outcome is dropped, matching what these routes already did with it: the
// sweep is best-effort on the reduction paths, the authority change has already
// committed, and turning a sweep failure into a 5xx would tell the caller their
// change did not apply when it did. EraseUser is the exception and supplies its
// own reducer, because there an incomplete sweep IS fatal.
func sweepReduced(creds *credlifecycle.Sweeper, reason string) approles.AuthorityReducer {
	return func(ctx context.Context, userID string) error {
		creds.AuthorityReduced(ctx, userID, reason)
		return nil
	}
}

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
		// The route's own guard (requireSharedOrgAdminWithTargetUser) has already
		// proved the caller shares an administered organization with the target;
		// the same set, as a store.OrgScope, is what the accessors now require, so
		// a target outside it reports ErrNotFound and keeps this route's 404.
		scope, sErr := h.callerOrgScope(c)
		if sErr != nil {
			serverError(c, sErr, "failed to resolve caller organizations")
			return
		}
		user, err := h.userRepo.GetUserByID(c.Request.Context(), c.Param("id"), scope)
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
		if err := h.userRepo.UpdateUser(c.Request.Context(), user, scope); err != nil {
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
		scope, sErr := h.callerOrgScope(c)
		if sErr != nil {
			serverError(c, sErr, "failed to resolve caller organizations")
			return
		}
		// Invalidate every credential family before removing the account: the
		// API-key rows (whose stored scopes keep authenticating until something
		// deletes them) and the user's live JWT sessions, whose scopes were
		// embedded at login and which the JTI denylist cannot reach because no
		// JTI is recorded (#330). identity v0.25.0's migration 000007 makes
		// api_keys.user_id ON DELETE CASCADE, which is a backstop for the rows —
		// it still cannot reach a live session, so the sweep still runs first.
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
		deleted := true
		if err := h.userRepo.DeleteUser(ctx, id, scope); err != nil {
			if !notFound(err) {
				serverError(c, err, "failed to delete user")
				return
			}
			deleted = false
		}
		// identity.organization_members.user_id is ON DELETE CASCADE, so a
		// successful delete withdrew every role this principal held — without any
		// membership statement of its own, and therefore without passing through
		// the mirror on h.orgRepo. TSM's own organization_member_roles has no
		// foreign key to cascade with (identity may be another database), so the
		// cascade is mirrored explicitly.
		//
		// ONLY WHEN THE DELETE ACTUALLY APPLIED, and that is a tenancy guard, not
		// tidiness. DeleteUser is scoped: a caller acting on a user outside their
		// organizations matches no row and returns ErrNotFound, which this route
		// absorbs into its idempotent 204. PurgeUserRoles is deliberately
		// unscoped — it strips a principal whose identity row is gone, so there
		// is no organization left to test — so calling it on that absorbed
		// ErrNotFound would let a caller wipe another tenant's mirrored roles
		// with a request that changed nothing in identity and reported success.
		// Nothing reads the mirror in Phase 3a, so it would go unnoticed until
		// the phase that does.
		//
		// Rows left behind by a user who was already gone are collected by the
		// startup reconcile (approles.Reconcile's sweep), which is the mechanism
		// for exactly that and needs no privilege from this request.
		//
		// PLATFORM-WIDE, matching the CASCADE it mirrors: identity removed this
		// principal's memberships in EVERY organization, not just the caller's,
		// so a narrower strip would leave the mirror describing memberships that
		// no longer exist. Reaching that far is safe only because the guard above
		// has established that the scoped delete actually applied — the scope was
		// enforced on the delete, and this follows it.
		if deleted {
			h.orgRepo.PurgeUserRoles(ctx, id, idstore.OrgScopeAllOrganizations())
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
// membershipsInCallerScope filters a target user's memberships down to the
// organizations the caller actually administers.
//
// requireSharedOrgAdminWithTargetUser proves the caller administers AT LEAST
// ONE organization the target belongs to. That authorises asking about the
// user; it does not authorise seeing every organization the user belongs to.
// Returning the unfiltered list hands an org-A admin the target's membership in
// orgs B, C and D — organizations the caller belongs to nowhere.
//
// The shared module names this explicitly. store.GetUserMemberships is
// documented "UNSCOPED BY DESIGN — authority derivation": it is what
// OrgScopeForUser itself reads to work out where a principal may act, so a
// scope parameter would be circular. Its doc then says the consumer must guard
// it when asking about SOMEONE ELSE, which is what this does.
//
// terraform-registry-backend already filters the equivalent endpoint the same
// way. Same data, same accessor, guarded in one consumer and not the other.
func membershipsInCallerScope(scope idstore.OrgScope, memberships []*idmodels.UserMembership) []*idmodels.UserMembership {
	if scope.IsAllOrganizations() {
		return memberships
	}
	permitted := make([]*idmodels.UserMembership, 0, len(memberships))
	for _, m := range memberships {
		if scope.PermitsOrganization(m.OrganizationID) {
			permitted = append(permitted, m)
		}
	}
	return permitted
}

func (h *AdminHandlers) GetUserMemberships() gin.HandlerFunc {
	return func(c *gin.Context) {
		// GUARD user-memberships-caller-scope (identity #183).
		scope, err := h.callerScopeFor(c, idauth.ScopeAdmin)
		if err != nil {
			serverError(c, err, "failed to resolve caller organizations")
			return
		}
		memberships, err := h.orgRepo.GetUserMemberships(c.Request.Context(), c.Param("id"))
		if err != nil {
			serverError(c, err, "failed to load memberships")
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"memberships": membershipsInCallerScope(scope, memberships),
		})
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
		// One scope for the whole export: the organizations the caller
		// administers, plus rows nobody owns. It bounds the subject lookup and
		// the audit read alike, so the export cannot disclose through one axis
		// what it refuses on the other.
		scope, err := h.callerOrgScope(c)
		if err != nil {
			serverError(c, err, "failed to resolve caller organizations")
			return
		}
		user, err := h.userRepo.GetUserByID(ctx, id, scope)
		if err != nil || user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		memberships, err := h.orgRepo.GetUserMemberships(ctx, id)
		if err != nil {
			serverError(c, err, "failed to load memberships")
			return
		}
		// Same narrowing the audit read below already applies, for the same
		// reason (identity #183). It was applied to the trail and not to the
		// memberships shipped beside it in the same document.
		memberships = membershipsInCallerScope(scope, memberships)
		// Audit entries attributed to the user (capped — exports are not a
		// general audit-archival mechanism).
		//
		// GUARD audit-scope-user-export (#331): scoped to the CALLER, not the
		// target. requireSharedOrgAdminWithTargetUser only proves the caller
		// administers at least ONE organization the target belongs to, so an
		// unscoped read would hand the caller the target's trail from every
		// OTHER tenant the target also belongs to — the same leak as the bulk
		// export, reached through the per-user axis.
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
		scope, sErr := h.callerOrgScope(c)
		if sErr != nil {
			serverError(c, sErr, "failed to resolve caller organizations")
			return
		}
		user, err := h.userRepo.GetUserByID(ctx, id, scope)
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
		if err := h.userRepo.UpdateUser(ctx, user, scope); err != nil {
			if notFound(err) {
				c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
				return
			}
			serverError(c, err, "failed to anonymize user")
			return
		}
		// The bulk sweep reports the organizations it emptied rather than
		// ErrNotFound: a user who holds no memberships is already in the erased
		// end state.
		//
		// Erasure is deliberately NOT narrowed to the caller's tenancy. Article
		// 17 is an obligation about the whole data subject, and the two steps
		// either side of this one — anonymizing the users row and revoking every
		// credential — are already whole-principal. A tenant-scoped strip here
		// would leave the "erased" account still a live member elsewhere, which
		// is both a compliance failure and an authority the anonymized row can
		// still be used to exercise. See admin_audit_scope_test.go, which
		// records this as a reviewed platform-wide call site.
		// GDPR erasure keeps the user row as a resolvable tombstone, so both
		// credential families survive the membership strip: a still-valid API key
		// would keep authenticating at its stored scopes, and a live session
		// would keep serving the scope union just removed (#330). Supplied as the
		// strip's reducer so the two cannot come apart — and unlike the other
		// reduction routes, an INCOMPLETE sweep is fatal here, because "erased"
		// is a compliance claim about credentials as well as rows.
		var out credlifecycle.Outcome
		removed, err := h.orgRepo.RemoveAllMembershipsForUser(ctx, id, idstore.OrgScopeAllOrganizations(),
			func(ctx context.Context, uid string) error {
				out = h.creds.UserDeprovisioned(ctx, uid, "admin: user erased (GDPR)")
				if out.Incomplete {
					return errCredentialSweepIncomplete
				}
				return nil
			})
		if errors.Is(err, errCredentialSweepIncomplete) {
			serverError(c, err, "failed to revoke credentials")
			return
		}
		if err != nil {
			serverError(c, err, "failed to revoke memberships")
			return
		}
		h.writeAudit(c, "user.erase", "user", id, map[string]interface{}{
			"api_keys_revoked": out.KeysRevoked, "sessions_revoked": out.TokensRevoked,
			"organizations_emptied": removed.OrganizationIDs()})
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
		if err := h.orgRepo.Create(ctx, org); err != nil {
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
				if err := h.orgRepo.AddMemberWithParams(ctx, org.ID, uid, "org_owner", idstore.OrgScopeOrganizations(org.ID)); err != nil {
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
		scope := routeOrgScope(c)
		org, err := h.orgRepo.GetByID(ctx, c.Param("id"), scope)
		if err != nil || org == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}
		if name := strings.TrimSpace(req.Name); name != "" && name != org.Name {
			if err := h.orgRepo.Rename(ctx, org.ID, name, scope); err != nil {
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
		if err := h.orgRepo.Update(ctx, org, scope); err != nil {
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
		// reduction with no membership statement of its own. The snapshot that
		// makes those members sweepable after the cascade now lives inside
		// Members.Delete, so "list before you delete" is no longer a convention
		// this route has to remember (#330).
		//
		// Idempotent DELETE (see DeleteUser): an already-absent organization
		// answered 204 before v0.24.0 and keeps answering 204.
		scope := routeOrgScope(c)
		if err := h.orgRepo.Delete(ctx, id, scope,
			sweepReduced(h.creds, "admin: organization deleted")); err != nil && !notFound(err) {
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
		members, err := h.orgRepo.ListMembersWithUsers(c.Request.Context(), c.Param("id"), routeOrgScope(c))
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
		if err := h.orgRepo.AddMemberWithRoleTemplate(c.Request.Context(), orgID, req.UserID, roleID, routeOrgScope(c)); err != nil {
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
		// A reassignment can narrow what the member holds, and both credential
		// families froze the previous scope set. The sweep is the write's own
		// argument now, and still runs after it, so the retained authority is
		// re-derived from the new role template (#330).
		if err := h.orgRepo.UpdateMemberRoleTemplate(ctx, orgID, userID, roleID, routeOrgScope(c),
			sweepReduced(h.creds, "admin: organization member role changed")); err != nil && !notFound(err) {
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
		ctx := c.Request.Context()
		// Idempotent DELETE (see DeleteUser): removing a user who is not a
		// member answered 204 before v0.24.0 and keeps answering 204. The sweep
		// below still runs, which is the safe direction — it recomputes the
		// authority the user retains rather than trusting that a row moved.
		// The membership is gone; the credentials minted while it existed are
		// not. The sweep is the removal's own argument, against the authority the
		// user retains in their remaining organizations (#330).
		if err := h.orgRepo.RemoveMember(ctx, orgID, userID, routeOrgScope(c),
			sweepReduced(h.creds, "admin: organization membership removed")); err != nil && !notFound(err) {
			serverError(c, err, "failed to remove member")
			return
		}
		h.writeOrgAudit(c, "organization.member.remove", "organization", orgID, orgID, map[string]interface{}{"user_id": userID})
		c.Status(http.StatusNoContent)
	}
}
