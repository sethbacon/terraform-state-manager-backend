// Package webhooks implements the HTTP handlers for webhook-triggered operations.
package webhooks

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Handlers provides the HTTP handlers for webhook API endpoints.
type Handlers struct {
	sourceRepo      *repositories.StateSourceRepository
	analysisRunRepo *repositories.AnalysisRunRepository
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	sourceRepo *repositories.StateSourceRepository,
	analysisRunRepo *repositories.AnalysisRunRepository,
) *Handlers {
	return &Handlers{
		sourceRepo:      sourceRepo,
		analysisRunRepo: analysisRunRepo,
	}
}

// triggerRequest is the request body for the TriggerAnalysis endpoint.
type triggerRequest struct {
	SourceID string `json:"source_id" binding:"required"`
}

// TriggerAnalysis handles POST /api/v1/webhooks/trigger.
// Takes a source_id in the request body, validates it, creates a new analysis
// run, and returns the run_id.
//
// @Summary      Trigger webhook
// @Tags         Webhooks
// @Accept       json
// @Produce      json
// @Param        body  body      triggerRequest  true  "Webhook trigger request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /webhooks/trigger [post]
func (h *Handlers) TriggerAnalysis(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req triggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	if _, err := uuid.Parse(req.SourceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source_id"})
		return
	}

	ctx := c.Request.Context()

	// Verify the source exists and belongs to the organization.
	source, err := h.sourceRepo.GetByID(ctx, req.SourceID)
	if err != nil {
		slog.Error("Failed to get source for webhook trigger", "error", err, "source_id", req.SourceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve source"})
		return
	}
	if source == nil || source.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}

	// Create a new analysis run triggered via webhook/API.
	run := &models.AnalysisRun{
		OrganizationID: orgIDStr,
		SourceID:       &req.SourceID,
		Status:         models.RunStatusPending,
		TriggerType:    models.TriggerAPI,
		Config:         json.RawMessage("{}"),
	}

	if err := h.analysisRunRepo.Create(ctx, run); err != nil {
		slog.Error("Failed to create analysis run from webhook", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create analysis run"})
		return
	}

	slog.Info("Analysis run triggered via webhook",
		"run_id", run.ID, "source_id", req.SourceID, "org_id", orgIDStr)

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"run_id":    run.ID,
			"source_id": req.SourceID,
			"status":    run.Status,
			"message":   "analysis run created successfully",
		},
	})
}
