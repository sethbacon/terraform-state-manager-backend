// platform_admins.go serves the management surface for TSM's platform-admin
// carrier: who administers THIS deployment, who conferred it, when, and why.
//
// # Why there is an API at all
//
// The carrier could have been populated by the first-run wizard alone. That
// would have left every deployment that upgrades into this migration — setup
// long since completed and the wizard permanently burnt — with an empty carrier
// and no way to fill it short of hand-written SQL against the very table this
// mechanism exists to replace. Hand-written SQL is also the one caller that can
// forget the audit intent; the constraint trigger refuses it, which is correct
// but is a poor operator experience for the highest privilege in the product.
//
// # Where the refusals come from
//
// Every one of them is the shared module's, surfaced verbatim rather than
// re-derived here, and they map onto DIFFERENT statuses on purpose:
//
//	ErrAlreadyPlatformAdmin  409  the row exists; its provenance was left alone
//	ErrNotPlatformAdmin      404  there was no row to remove
//	ErrLastPlatformAdmin     409  there is genuinely nobody else — grant first
//	ErrIdentityUnavailable   503  ask again later; nothing was changed
//	ErrUnknownUser           400  the id answers to nobody
//
// Collapsing "there is nobody else" and "I could not find out whether there is
// anybody else" onto one status is exactly the conflation the module's two
// sentinels exist to prevent: the first is a conflict an operator resolves by
// granting somebody, the second is an outage during which the last real
// administrator must NOT be allowed to remove themselves.
//
// # Where this contract diverges from the sibling registry's, and why
//
// Registry serves the same concept at the same path, so a client author will
// reasonably assume the two agree. On four points they do not, and each is a
// decision recorded here rather than a drift to be discovered by diffing two
// repositories (#392):
//
//	orphan flag           TSM `orphaned: true`   registry `user_resolved: false`
//	unknown grant target  TSM 400                registry 404
//	identity unreachable  TSM 503                registry 500
//	note length           TSM unvalidated        registry capped at 500
//
// The ORPHAN FLAG is the sharpest: same idea, different key AND inverted sense,
// so a client reading the wrong one gets the exact opposite answer rather than
// an obviously missing field. TSM keeps its spelling — see platformAdminView —
// because flipping it is precisely the change an existing client cannot notice.
//
// UNKNOWN TARGET is 400 because this endpoint takes the id in a request BODY it
// is validating; the request is malformed in the sense that matters, and 404
// would be a statement about the route. IDENTITY UNREACHABLE is 503 rather than
// 500 because nothing was changed, the answer is unresolved rather than
// negative, and retrying is the correct client behaviour — which is the same
// reasoning the refusal table above already applies.
//
// The NOTE LENGTH divergence is the one genuinely worth closing, and is left
// open on purpose: a cap newly refuses requests that succeed today, which is a
// change to what this mutation accepts and belongs in its own change rather
// than riding along with a read-side one. The column is TEXT (migration 000030)
// and the route is behind the admin scope, so the exposure is an operator
// writing an over-long note, not an unauthenticated one.
package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	idplatformadmin "github.com/sethbacon/terraform-suite-identity/identity/platformadmin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth"
	"github.com/terraform-state-manager/terraform-state-manager/internal/platformadmin"
)

// PlatformAdminHandlers serves /api/v1/admin/platform-admins.
type PlatformAdminHandlers struct {
	svc *platformadmin.Service
}

// NewPlatformAdminHandlers constructs the handlers over the carrier service. A
// nil service is legal and answers 503: the routes still exist, so an operator
// gets "the carrier is not wired up here" instead of a 404 that reads as "this
// build has no platform admins".
func NewPlatformAdminHandlers(svc *platformadmin.Service) *PlatformAdminHandlers {
	return &PlatformAdminHandlers{svc: svc}
}

// actorOf captures the acting principal AS THE REQUEST KNEW IT.
//
// The email is read from the session claims rather than looked up: the outbox
// may deliver this record minutes later, identity may be another database, and
// the audit entry has to stay attributable after the user row is gone. Nothing
// downstream can recover an address that was true at the time.
func actorOf(c *gin.Context) platformadmin.Actor {
	actor := platformadmin.Actor{UserID: userIDOf(c), IPAddress: c.ClientIP()}
	if v, ok := c.Get("jwt_claims"); ok {
		if claims, ok := v.(*auth.Claims); ok {
			actor.Email = claims.Email
		}
	}
	return actor
}

// carrierError maps a module sentinel onto a status. Returns false when err is
// not one of them, so the caller answers 500 rather than inventing a meaning.
func carrierError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, platformadmin.ErrUnknownUser):
		c.JSON(http.StatusBadRequest, gin.H{"error": "no user with that id"})
	case errors.Is(err, idplatformadmin.ErrAlreadyPlatformAdmin):
		c.JSON(http.StatusConflict, gin.H{"error": "that user is already a platform administrator"})
	case errors.Is(err, idplatformadmin.ErrNotPlatformAdmin):
		c.JSON(http.StatusNotFound, gin.H{"error": "that user is not a platform administrator"})
	case errors.Is(err, idplatformadmin.ErrLastPlatformAdmin):
		c.JSON(http.StatusConflict, gin.H{
			"error": "the last platform administrator cannot be revoked; grant another one first"})
	case errors.Is(err, idplatformadmin.ErrIdentityUnavailable):
		// 503, not 403 and not 500. Nothing was changed, the answer is
		// unresolved rather than negative, and the correct client behaviour is
		// to retry — which a 500 does not communicate and a 403 actively
		// misreports.
		upstreamError(c, http.StatusServiceUnavailable, err,
			"the identity store could not be reached, so this change was not attempted")
	case errors.Is(err, idplatformadmin.ErrNotSerialized):
		upstreamError(c, http.StatusServiceUnavailable, err,
			"the platform-admin lock could not be taken, so this change was not attempted")
	default:
		return false
	}
	return true
}

// requireCarrier answers 503 when the carrier is not wired up, and reports
// whether the handler may proceed.
func (h *PlatformAdminHandlers) requireCarrier(c *gin.Context) bool {
	if h == nil || h.svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "the platform-admin carrier is not configured on this deployment"})
		return false
	}
	return true
}

// platformAdminView is one carrier row on the wire.
//
// Email and Name are the GRANTEE; GrantedByEmail is whoever conferred the grant.
// They are omitempty because "not resolved" is a real state here and an empty
// string is not a person: an orphaned grant carries neither, and a grant whose
// granter has since been deleted keeps its granted_by UUID with no address
// beside it. UserID, GrantedBy, GrantedAt and Note are always present, so a row
// never loses the identifiers an operator needs to act on it.
type platformAdminView struct {
	UserID string `json:"user_id"`
	Email  string `json:"email,omitempty"`
	Name   string `json:"name,omitempty"`
	// GrantedBy is NULL for the first-boot bootstrap: nobody conferred that row,
	// and Note records where it came from.
	GrantedBy      *string `json:"granted_by"`
	GrantedByEmail string  `json:"granted_by_email,omitempty"`
	GrantedAt      string  `json:"granted_at"`
	Note           *string `json:"note"`
	// Orphaned marks a grant whose principal no longer resolves. Such a row
	// elevates NOBODY and is not counted by the never-zero floor, but it is
	// still in the table and this listing is the only surface that can remove
	// it — which is why it is labelled here rather than filtered out.
	//
	// POLARITY. The sibling registry spells this same idea as `user_resolved`,
	// with the opposite sense. TSM keeps `orphaned` deliberately: flipping it
	// would leave every existing client reading a field that is still present,
	// still boolean, and now says the exact opposite — the one shape of change a
	// client cannot notice. Converging on one spelling is worth doing, but as a
	// declared breaking change with an upgrade note, not as a side effect of
	// adding identities.
	Orphaned bool `json:"orphaned"`
}

// ListPlatformAdmins lists the platform administrators of this deployment.
// @Summary      List platform administrators
// @Description  Returns every platform-admin grant with its provenance, resolved to people: `email` and `name` for the grantee, `granted_by_email` for whoever conferred it. A grant whose user no longer resolves is returned with `orphaned: true` and no identity fields rather than hidden — it is the only surface such a row can be removed from. Identity may live in a separate database, so it is resolved per request rather than joined; when it cannot be reached the endpoint answers 503 and lists nothing, because an administrator list that silently reports live administrators as orphans invites an operator to delete them.
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/platform-admins [get]
func (h *PlatformAdminHandlers) ListPlatformAdmins() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.requireCarrier(c) {
			return
		}
		entries, err := h.svc.List(c.Request.Context())
		if err != nil {
			if carrierError(c, err) {
				return
			}
			serverError(c, err, "failed to list platform administrators")
			return
		}
		out := make([]platformAdminView, 0, len(entries))
		for _, e := range entries {
			view := platformAdminView{
				UserID:    e.UserID,
				GrantedBy: e.GrantedBy,
				GrantedAt: e.GrantedAt.UTC().Format(time.RFC3339),
				Note:      e.Note,
				Orphaned:  !e.Exists,
			}
			// Absent, not blank. A grant nobody answers to keeps its row and its
			// flag and gains no half-filled person; the service resolved the
			// identity and the existence flag from the SAME lookup, so the two
			// cannot disagree about whether there is somebody there.
			if e.User != nil {
				view.Email, view.Name = e.User.Email, e.User.Name
			}
			if e.Granter != nil {
				view.GrantedByEmail = e.Granter.Email
			}
			out = append(out, view)
		}
		c.JSON(http.StatusOK, gin.H{"platform_admins": out, "total": len(out)})
	}
}

type platformAdminGrantRequest struct {
	UserID string `json:"user_id" binding:"required"`
	Note   string `json:"note"`
}

// GrantPlatformAdmin confers platform-admin authority on a user.
// @Summary      Grant platform admin
// @Description  Records a platform-admin grant, with the acting principal and an optional note as provenance. The audit record commits in the same transaction as the grant.
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/platform-admins [post]
func (h *PlatformAdminHandlers) GrantPlatformAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.requireCarrier(c) {
			return
		}
		var req platformAdminGrantRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required"})
			return
		}
		// Parsed here so a malformed id is a 400 rather than a driver error the
		// resolver would have to report as an identity outage (503).
		if _, err := uuid.Parse(strings.TrimSpace(req.UserID)); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user_id must be a UUID"})
			return
		}
		var note *string
		if trimmed := strings.TrimSpace(req.Note); trimmed != "" {
			note = &trimmed
		}
		grant, err := h.svc.Grant(c.Request.Context(), req.UserID, actorOf(c), note)
		if err != nil {
			if carrierError(c, err) {
				return
			}
			serverError(c, err, "failed to grant platform admin")
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"user_id":    grant.UserID,
			"granted_by": grant.GrantedBy,
			"granted_at": grant.GrantedAt.UTC().Format(time.RFC3339),
			"note":       grant.Note,
		})
	}
}

// RevokePlatformAdmin removes a platform-admin grant.
// @Summary      Revoke platform admin
// @Description  Removes a platform-admin grant. Refused when it would leave the deployment with no administrator who could actually exercise the privilege.
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /admin/platform-admins/{user_id} [delete]
func (h *PlatformAdminHandlers) RevokePlatformAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.requireCarrier(c) {
			return
		}
		userID := strings.TrimSpace(c.Param("user_id"))
		// A malformed id can never name a row (user_id is UUID), so this is a
		// 404 about the grant rather than a 400 about the request.
		if _, err := uuid.Parse(userID); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "that user is not a platform administrator"})
			return
		}
		grant, err := h.svc.Revoke(c.Request.Context(), userID, actorOf(c))
		if err != nil {
			if carrierError(c, err) {
				return
			}
			serverError(c, err, "failed to revoke platform admin")
			return
		}
		c.JSON(http.StatusOK, gin.H{"user_id": grant.UserID, "revoked": true})
	}
}
