// Package notifications implements the HTTP handlers for notification channel management.
package notifications

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notification"
)

// Handlers provides the HTTP handlers for notification channel API endpoints.
type Handlers struct {
	channelRepo     *repositories.NotificationChannelRepository
	notificationSvc *notification.Service
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(
	channelRepo *repositories.NotificationChannelRepository,
	notificationSvc *notification.Service,
) *Handlers {
	return &Handlers{
		channelRepo:     channelRepo,
		notificationSvc: notificationSvc,
	}
}

// ---------------------------------------------------------------------------
// Channel handlers
// ---------------------------------------------------------------------------

// ListChannels handles GET /api/v1/notifications/channels.
// Returns a paginated list of notification channels for the authenticated user's organization.
func (h *Handlers) ListChannels(c *gin.Context) {
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
	channels, total, err := h.channelRepo.ListByOrganization(ctx, orgIDStr, limit, offset)
	if err != nil {
		slog.Error("Failed to list notification channels", "error", err, "org_id", orgIDStr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list notification channels"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":   channels,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// CreateChannel handles POST /api/v1/notifications/channels.
// Creates a new notification channel for the authenticated user's organization.
func (h *Handlers) CreateChannel(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	var req models.NotificationChannelCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	channel := &models.NotificationChannel{
		OrganizationID: orgIDStr,
		Name:           req.Name,
		ChannelType:    req.ChannelType,
		Config:         req.Config,
		IsActive:       isActive,
	}

	ctx := c.Request.Context()
	if err := h.channelRepo.Create(ctx, channel); err != nil {
		slog.Error("Failed to create notification channel", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create notification channel"})
		return
	}

	slog.Info("Notification channel created",
		"channel_id", channel.ID, "name", channel.Name, "type", channel.ChannelType)

	c.JSON(http.StatusCreated, gin.H{"data": channel})
}

// GetChannel handles GET /api/v1/notifications/channels/:id.
// Returns a single notification channel by ID.
func (h *Handlers) GetChannel(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel ID"})
		return
	}

	ctx := c.Request.Context()
	channel, err := h.channelRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get notification channel", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve notification channel"})
		return
	}
	if channel == nil || channel.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "notification channel not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": channel})
}

// UpdateChannel handles PUT /api/v1/notifications/channels/:id.
// Applies partial updates to a notification channel.
func (h *Handlers) UpdateChannel(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel ID"})
		return
	}

	var req models.NotificationChannelUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid input",
			"details": err.Error(),
		})
		return
	}

	ctx := c.Request.Context()
	channel, err := h.channelRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get notification channel for update", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve notification channel"})
		return
	}
	if channel == nil || channel.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "notification channel not found"})
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		channel.Name = *req.Name
	}
	if req.ChannelType != nil {
		channel.ChannelType = *req.ChannelType
	}
	if req.Config != nil {
		channel.Config = *req.Config
	}
	if req.IsActive != nil {
		channel.IsActive = *req.IsActive
	}

	if err := h.channelRepo.Update(ctx, channel); err != nil {
		slog.Error("Failed to update notification channel", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update notification channel"})
		return
	}

	slog.Info("Notification channel updated", "channel_id", id)
	c.JSON(http.StatusOK, gin.H{"data": channel})
}

// DeleteChannel handles DELETE /api/v1/notifications/channels/:id.
func (h *Handlers) DeleteChannel(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel ID"})
		return
	}

	ctx := c.Request.Context()
	channel, err := h.channelRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get notification channel for deletion", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve notification channel"})
		return
	}
	if channel == nil || channel.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "notification channel not found"})
		return
	}

	if err := h.channelRepo.Delete(ctx, id); err != nil {
		slog.Error("Failed to delete notification channel", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete notification channel"})
		return
	}

	slog.Info("Notification channel deleted", "channel_id", id, "name", channel.Name)
	c.JSON(http.StatusOK, gin.H{"message": "notification channel deleted successfully"})
}

// TestChannel handles POST /api/v1/notifications/channels/:id/test.
// Sends a test notification through the specified channel.
func (h *Handlers) TestChannel(c *gin.Context) {
	orgID, _ := c.Get("organization_id")
	orgIDStr, ok := orgID.(string)
	if !ok || orgIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id not found in context"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel ID"})
		return
	}

	ctx := c.Request.Context()
	channel, err := h.channelRepo.GetByID(ctx, id)
	if err != nil {
		slog.Error("Failed to get notification channel for testing", "error", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve notification channel"})
		return
	}
	if channel == nil || channel.OrganizationID != orgIDStr {
		c.JSON(http.StatusNotFound, gin.H{"error": "notification channel not found"})
		return
	}

	if err := h.notificationSvc.SendTest(ctx, channel); err != nil {
		slog.Error("Failed to send test notification", "error", err, "channel_id", id)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  "failed to send test notification",
			"detail": err.Error(),
		})
		return
	}

	slog.Info("Test notification sent", "channel_id", id, "channel_type", channel.ChannelType)
	c.JSON(http.StatusOK, gin.H{"message": "test notification sent successfully"})
}
