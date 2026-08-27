package api

// legal_holds.go is the admin surface for audit legal holds (#373).
//
// PLACING A HOLD IS A PRIVILEGED ACT AND IS ITSELF AUDITED. It changes what the
// retention sweep may delete, so an unaudited hold would let someone shield
// evidence without leaving a record that they had — which is the same class of
// gap as an unaudited permission grant.
//
// Releasing is audited for the mirror reason: it re-exposes rows to the next
// sweep, and "who decided this evidence no longer needs preserving" is exactly
// the question an investigation asks.

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
	"github.com/terraform-state-manager/terraform-state-manager/internal/legalhold"
)

// LegalHoldHandlers serves the hold admin routes.
type LegalHoldHandlers struct {
	repo  *legalhold.Repository
	audit *idstore.AuditRepository
}

// NewLegalHoldHandlers wires the handlers. A nil repo leaves every route
// answering 503 rather than panicking — the unit-test rig runs with no database.
func NewLegalHoldHandlers(repo *legalhold.Repository, audit *idstore.AuditRepository) *LegalHoldHandlers {
	return &LegalHoldHandlers{repo: repo, audit: audit}
}

func (h *LegalHoldHandlers) available(c *gin.Context) bool {
	if h == nil || h.repo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "legal holds are not available"})
		return false
	}
	return true
}

type placeHoldRequest struct {
	Name      string    `json:"name" binding:"required"`
	Reason    string    `json:"reason"`
	StartDate time.Time `json:"start_date" binding:"required"`
	EndDate   time.Time `json:"end_date" binding:"required"`
}

// PlaceHold records a hold.
//
// @Summary      Place an audit legal hold
// @Description  Records a date range whose audit entries the retention sweep must not delete. Placing a hold is itself audited.
// @Tags         Admin
// @Security     BearerAuth
// @Accept       json
// @Produce      json
// @Param        request  body      placeHoldRequest  true  "Hold to place"
// @Success      201      {object}  legalhold.Hold
// @Failure      400      {object}  map[string]interface{}  "Invalid body, or end_date before start_date"
// @Failure      503      {object}  map[string]interface{}  "Legal holds are not available"
// @Router       /admin/legal-holds [post]
func (h *LegalHoldHandlers) PlaceHold() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.available(c) {
			return
		}
		var req placeHoldRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		actor, _ := c.Get("user_id")
		actorID, _ := actor.(string)

		hold, err := h.repo.Place(c.Request.Context(), req.Name, req.Reason, req.StartDate, req.EndDate, actorID)
		if errors.Is(err, legalhold.ErrInvalidRange) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err != nil {
			serverError(c, err, "failed to place legal hold")
			return
		}
		// Audited BEFORE the response, so a hold that exists is a hold that was
		// recorded. The metadata carries the range because that is what
		// determines which evidence the hold protects.
		writeAuditEntry(c, h.audit, "audit.legal_hold.place", "legal_hold", hold.ID, map[string]interface{}{
			"name":       hold.Name,
			"reason":     hold.Reason,
			"start_date": hold.StartDate,
			"end_date":   hold.EndDate,
		})
		c.JSON(http.StatusCreated, hold)
	}
}

// ReleaseHold deactivates a hold.
//
// @Summary      Release an audit legal hold
// @Description  Deactivates a hold, re-exposing its date range to the retention sweep. The hold row is kept as the record of the decision. Releasing is itself audited.
// @Tags         Admin
// @Security     BearerAuth
// @Produce      json
// @Param        id   path      string  true  "Hold ID (UUID)"
// @Success      200  {object}  legalhold.Hold
// @Failure      404  {object}  map[string]interface{}  "No such hold"
// @Failure      503  {object}  map[string]interface{}  "Legal holds are not available"
// @Router       /admin/legal-holds/{id}/release [post]
func (h *LegalHoldHandlers) ReleaseHold() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.available(c) {
			return
		}
		actor, _ := c.Get("user_id")
		actorID, _ := actor.(string)

		hold, err := h.repo.Release(c.Request.Context(), c.Param("id"), actorID)
		if errors.Is(err, legalhold.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "no such legal hold"})
			return
		}
		if err != nil {
			serverError(c, err, "failed to release legal hold")
			return
		}
		writeAuditEntry(c, h.audit, "audit.legal_hold.release", "legal_hold", hold.ID, map[string]interface{}{
			"name":       hold.Name,
			"start_date": hold.StartDate,
			"end_date":   hold.EndDate,
		})
		c.JSON(http.StatusOK, hold)
	}
}

// ListHolds returns every hold, active and released.
//
// Released holds are included deliberately: the record of a decision to stop
// preserving is as much a part of the audit story as the decision to start.
//
// @Summary      List audit legal holds
// @Description  Every hold, active and released, newest first.
// @Tags         Admin
// @Security     BearerAuth
// @Produce      json
// @Success      200  {array}   legalhold.Hold
// @Failure      503  {object}  map[string]interface{}  "Legal holds are not available"
// @Router       /admin/legal-holds [get]
func (h *LegalHoldHandlers) ListHolds() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.available(c) {
			return
		}
		holds, err := h.repo.List(c.Request.Context())
		if err != nil {
			serverError(c, err, "failed to list legal holds")
			return
		}
		c.JSON(http.StatusOK, holds)
	}
}
