package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/services/statesync"
)

// reconcileRequest is the optional body for POST /reconcile; an empty body (or
// empty source_ids) reconciles the whole fleet.
type reconcileRequest struct {
	SourceIDs []string `json:"source_ids"`
}

// ReconcileSources runs a state-sync reconcile cycle, scoped to the given
// source_ids (or all sources when none are given). It is the CSRF-protected POST
// replacement for the ?refresh=true side effect that previously ran on the GET
// dashboard/report endpoints: a reconcile re-reads state from every selected
// backend and rewrites the analysis store, so it is a state-changing action that
// must not be triggerable by a replayable cross-site GET (#215). The dashboard
// and report read endpoints stay pure GETs and simply serve the current store.
// @Summary      Reconcile source state
// @Description  Runs a state-sync reconcile cycle, scoped to the given source_ids (or all sources when omitted). Idempotent — returns immediately if a cycle is already running. Requires state:read.
// @Tags         Dashboard
// @Accept       json
// @Produce      json
// @Param        request  body      reconcileRequest  false  "Optional source_ids to scope the reconcile to specific sources"
// @Success      200      {object}  map[string]interface{}
// @Failure      500      {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /reconcile [post]
func (h *SourcesHandlers) ReconcileSources() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.syncer == nil {
			c.JSON(http.StatusOK, gin.H{"status": "disabled"})
			return
		}
		var req reconcileRequest
		// The body is optional; a decode error (e.g. an empty body) just means
		// "reconcile everything".
		_ = c.ShouldBindJSON(&req)

		ctx := c.Request.Context()
		var err error
		if len(req.SourceIDs) > 0 {
			err = h.syncer.SyncSources(ctx, req.SourceIDs)
		} else {
			err = h.syncer.SyncAll(ctx)
		}
		switch {
		case err == nil, errors.Is(err, statesync.ErrSyncInProgress):
			// Already-running counts as success: a cycle is (or just was) refreshing
			// the store, which is all the caller wants.
			c.JSON(http.StatusOK, gin.H{"status": "reconciled"})
		default:
			serverError(c, err, "reconcile failed")
		}
	}
}
