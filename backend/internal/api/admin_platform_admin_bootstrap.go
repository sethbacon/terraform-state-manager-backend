// admin_platform_admin_bootstrap.go carries the ONE place where the
// platform-admin carrier may stand in for organization membership (#485).
//
// THE DEADLOCK IT BREAKS. A platform admin who is not a member of an
// organization could not administer it, and could not become a member either:
// the organization directory is scoped to their memberships so the target does
// not appear; the group-mapping organization field is populated from that same
// list; a mapping cannot yield `admin` because guardProvisionableRole refuses
// it; and a membership inserted directly is revoked by the IdP reconciler on
// their next authenticated request. What actually worked was an accident — the
// organization field is a freeSolo Autocomplete storing a name string, so an
// admin could type an organization that was not offered. That is one
// accessibility fix away from disappearing, and then there is no recovery.
//
// WHY THE BYPASS IS THIS NARROW. `admin` is granted per organization in TSM and
// merely SURFACES as a flat scope, so "the caller holds admin" does NOT mean
// "the caller is platform-wide" — every /admin caller is somebody's tenant
// admin. Widening on the flat scope would hand every single-organization admin
// the whole directory. The carrier is the only thing that answers the right
// question, and it is asked directly.
//
// The cross-tenant READ surface is deliberately untouched: audit lists, the
// CSV/JSON export, the GDPR export and user search all still derive scope from
// membership via callerScopeFor, and TestNoPlatformWideOrgScopeInAuditHandlers
// still fails the build if an OrgScopeAllOrganizations() reaches them.
package api

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"
)

// platformAdminSource is the narrow question this file asks of the carrier.
// Declared as an interface so a test can answer it directly, including with an
// error -- the case that must NOT be read as "is a platform admin".
type platformAdminSource interface {
	IsPlatformAdmin(ctx context.Context, userID string) (bool, error)
}

// platformAdminBootstrapRoutes are the exact method+route pairs where the
// carrier may substitute for membership. Keyed on METHOD as well as path
// because GET and POST on /:id/members share a route template, and only the
// write is a bootstrap step -- listing another tenant's members is a read
// widening this change does not make.
var platformAdminBootstrapRoutes = map[string]bool{
	"POST /api/v1/admin/organizations/:id/members": true,
}

// callerIsPlatformAdmin reports whether THIS REQUEST is presented by a
// platform admin, and is deliberately stricter than "the user is one".
//
// SESSION ONLY. A key is refused even when its owner is a platform admin,
// because a key is a narrowed credential and its owner's standing is not a
// statement about it -- the same distinction requireOrgScope already draws when
// it checks the presented credential before the user's role rows. Belt and
// braces: idplatformadmin.KeyScopes already strips `admin` from every key, so a
// key cannot reach a route gated on that scope; this check means the bypass
// does not RELY on that being true forever.
//
// FAILS CLOSED. A carrier that cannot be reached answers "not a platform
// admin", so an outage narrows what a caller may do rather than widening it.
func (h *AdminHandlers) callerIsPlatformAdmin(c *gin.Context) bool {
	if h.platformAdmins == nil {
		return false
	}
	if method, _ := c.Get("auth_method"); method == "apikey" {
		return false
	}
	uid, _ := c.Get("user_id")
	userID, _ := uid.(string)
	if userID == "" {
		return false
	}
	isAdmin, err := h.platformAdmins.IsPlatformAdmin(c.Request.Context(), userID)
	if err != nil {
		slog.Warn("could not determine platform-admin standing; treating the caller as an ordinary tenant admin",
			"user_id", userID, "path", c.FullPath(), "error", err)
		return false
	}
	return isAdmin
}

// platformAdminMayBootstrap reports whether this request is one of the narrow
// bootstrap routes AND is presented by a platform admin.
func (h *AdminHandlers) platformAdminMayBootstrap(c *gin.Context) bool {
	if !platformAdminBootstrapRoutes[c.Request.Method+" "+c.FullPath()] {
		return false
	}
	if !h.callerIsPlatformAdmin(c) {
		return false
	}
	slog.Info("platform admin bootstrapping access to an organization they are not a member of",
		"user_id", c.GetString("user_id"), "organization_id", c.Param("id"), "path", c.FullPath())
	return true
}
