// Package alerts implements the HTTP handlers for alert and alert rule management.
package alerts

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notification"
)

// Handlers provides the HTTP handlers for alert API endpoints.
type Handlers struct {
	alertRepo       *repositories.AlertsRepository
	ruleRepo        *repositories.AlertRuleRepository
	notificationSvc *notification.Service
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	alertRepo *repositories.AlertsRepository,
	ruleRepo *repositories.AlertRuleRepository,
	notificationSvc *notification.Service,
) *Handlers {
	return &Handlers{
		alertRepo:       alertRepo,
		ruleRepo:        ruleRepo,
		notificationSvc: notificationSvc,
	}
}

// ---------------------------------------------------------------------------
// Alert handlers
// ---------------------------------------------------------------------------

// ListAlerts handles GET /api/v1/alerts.
// Returns a paginated list of alerts for the authenticated user's organization.
// Query parameters: limit (default 20), offset (default 0), severity, acknowledged.
// @Summary      List alerts
// @Description  Returns a paginated list of alerts for the organization
// @Tags         Alerts
// @Produce      json
// @Param        limit        query  int     false  "Page size (default 20, max 100)"
// @Param        offset       query  int     false  "Page offset (default 0)"
// @Param        severity     query  string  false  "Filter by severity"
// @Param        acknowledged query  bool    false  "Filter by acknowledged status"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /alerts [get]
func (h *Handlers) ListAlerts(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	severity := c.Query("severity")

	var acknowledged *bool
	if ackStr := c.Query("acknowledged"); ackStr != "" {
		ackVal, err := strconv.ParseBool(ackStr)
		if err == nil {
			acknowledged = &ackVal
		}
	}

	ctx := c.Request.Context()
	alerts, total, err := h.alertRepo.ListByOrganization(ctx, orgIDStr, severity, acknowledged, limit, offset)
	if err != nil {
		slog.Error("Failed to list alerts", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list alerts"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   alerts,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// AcknowledgeAlert handles PUT /api/v1/alerts/:id/acknowledge.
// Marks an alert as acknowledged by the authenticated user.
// @Summary      Acknowledge alert
// @Description  Marks an alert as acknowledged by the authenticated user
// @Tags         Alerts
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /alerts/{id}/acknowledge [put]
func (h *Handlers) AcknowledgeAlert(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alert ID"})
		return
	}

	ctx := c.Request.Context()
	alert, err := h.alertRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get alert for acknowledgement", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve alert"})
		return
	}
	if alert == nil || alert.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
		return
	}

	if err := h.alertRepo.Acknowledge(ctx, id, userIDStr); err != nil {
		slog.Error("Failed to acknowledge alert", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to acknowledge alert"})
		return
	}

	slog.Info("Alert acknowledged", "alert_id", id, "user_id", userIDStr)
	c.JSON(http.StatusOK, gin.H{"message": "alert acknowledged successfully"})
}

// ---------------------------------------------------------------------------
// Alert rule handlers
// ---------------------------------------------------------------------------

// CreateAlertRule handles POST /api/v1/alerts/rules.
// Creates a new alert rule for the authenticated user's organization.
// @Summary      Create alert rule
// @Description  Creates a new alert rule for the organization
// @Tags         Alerts
// @Accept       json
// @Produce      json
// @Param        request  body      models.AlertRuleCreateRequest  true  "Alert rule create request"
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /alerts/rules [post]
func (h *Handlers) CreateAlertRule(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req models.AlertRuleCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	severity := "warning"
	if req.Severity != "" {
		severity = req.Severity
	}

	channelIDs := json.RawMessage("[]")
	if req.ChannelIDs != nil {
		channelIDs = req.ChannelIDs
	}

	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	rule := &models.AlertRule{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		RuleType:       req.RuleType,
		Config:         req.Config,
		Severity:       severity,
		ChannelIDs:     channelIDs,
		IsActive:       isActive,
	}

	ctx := c.Request.Context()
	if err := h.ruleRepo.Create(ctx, rule); err != nil {
		slog.Error("Failed to create alert rule", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create alert rule"})
		return
	}

	slog.Info("Alert rule created",
		"rule_id", rule.ID, "name", rule.Name, "type", rule.RuleType)

	c.JSON(http.StatusCreated, gin.H{"data": rule})
}

// ListAlertRules handles GET /api/v1/alerts/rules.
// Returns a paginated list of alert rules for the authenticated user's organization.
// @Summary      List alert rules
// @Description  Returns a paginated list of alert rules for the organization
// @Tags         Alerts
// @Produce      json
// @Param        limit   query  int  false  "Page size (default 20, max 100)"
// @Param        offset  query  int  false  "Page offset (default 0)"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /alerts/rules [get]
func (h *Handlers) ListAlertRules(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	ctx := c.Request.Context()
	rules, total, err := h.ruleRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list alert rules", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list alert rules"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   rules,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetAlertRule handles GET /api/v1/alerts/rules/:id.
// Returns a single alert rule by ID.
// @Summary      Get alert rule
// @Description  Returns a single alert rule by ID
// @Tags         Alerts
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /alerts/rules/{id} [get]
func (h *Handlers) GetAlertRule(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alert rule ID"})
		return
	}

	ctx := c.Request.Context()
	rule, err := h.ruleRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get alert rule", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve alert rule"})
		return
	}
	if rule == nil || rule.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert rule not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": rule})
}

// UpdateAlertRule handles PUT /api/v1/alerts/rules/:id.
// Applies partial updates to an alert rule.
// @Summary      Update alert rule
// @Description  Applies partial updates to an alert rule
// @Tags         Alerts
// @Accept       json
// @Produce      json
// @Param        id       path  string                         true  "Resource ID"
// @Param        request  body  models.AlertRuleUpdateRequest  true  "Alert rule update request"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /alerts/rules/{id} [put]
func (h *Handlers) UpdateAlertRule(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alert rule ID"})
		return
	}

	var req models.AlertRuleUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	ctx := c.Request.Context()
	rule, err := h.ruleRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get alert rule for update", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve alert rule"})
		return
	}
	if rule == nil || rule.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert rule not found"})
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		rule.Name = *req.Name
	}
	if req.RuleType != nil {
		rule.RuleType = *req.RuleType
	}
	if req.Config != nil {
		rule.Config = *req.Config
	}
	if req.Severity != nil {
		rule.Severity = *req.Severity
	}
	if req.ChannelIDs != nil {
		rule.ChannelIDs = *req.ChannelIDs
	}
	if req.IsActive != nil {
		rule.IsActive = *req.IsActive
	}

	if err := h.ruleRepo.Update(ctx, rule); err != nil {
		slog.Error("Failed to update alert rule", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update alert rule"})
		return
	}

	slog.Info("Alert rule updated", "rule_id", id)
	c.JSON(http.StatusOK, gin.H{"data": rule})
}

// DeleteAlertRule handles DELETE /api/v1/alerts/rules/:id.
// @Summary      Delete alert rule
// @Description  Deletes an alert rule by ID
// @Tags         Alerts
// @Produce      json
// @Param        id  path  string  true  "Resource ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Router       /alerts/rules/{id} [delete]
func (h *Handlers) DeleteAlertRule(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alert rule ID"})
		return
	}

	ctx := c.Request.Context()
	rule, err := h.ruleRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get alert rule for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve alert rule"})
		return
	}
	if rule == nil || rule.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert rule not found"})
		return
	}

	if err := h.ruleRepo.Delete(ctx, id); err != nil {
		slog.Error("Failed to delete alert rule", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete alert rule"})
		return
	}

	slog.Info("Alert rule deleted", "rule_id", id, "name", rule.Name)
	c.JSON(http.StatusOK, gin.H{"message": "alert rule deleted successfully"})
}
