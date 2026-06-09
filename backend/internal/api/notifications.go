// notifications.go implements CRUD + a test action for notification channels
// (alert destinations). Target URLs are capability-bearing secrets, so they are
// encrypted at rest (like pipeline tokens) and never returned by the API.
package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
)

var validChannelTypes = map[string]bool{"webhook": true, "slack": true}
var validEvents = map[string]bool{notify.EventDriftDetected: true, notify.EventRunFailed: true}

// NotificationHandlers serves the notification-channel endpoints.
type NotificationHandlers struct {
	repo     *repositories.NotificationChannelRepository
	notifier *notify.Notifier
}

// NewNotificationHandlers builds the handlers over the app connection.
func NewNotificationHandlers(database *sql.DB, notifier *notify.Notifier) *NotificationHandlers {
	return &NotificationHandlers{repo: repositories.NewNotificationChannelRepository(database), notifier: notifier}
}

type channelRequest struct {
	Name    string   `json:"name" binding:"required"`
	Type    string   `json:"type" binding:"required"`
	Target  string   `json:"target"` // destination URL; write-only (omit on edit to keep existing)
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled"`
}

// validate checks the type, events, and (when present) the target URL.
func (req *channelRequest) validate() error {
	if !validChannelTypes[req.Type] {
		return fmt.Errorf("type must be \"webhook\" or \"slack\"")
	}
	for _, e := range req.Events {
		if !validEvents[e] {
			return fmt.Errorf("unknown event %q (allowed: drift_detected, run_failed)", e)
		}
	}
	if req.Target != "" {
		u, err := url.Parse(req.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("target must be a valid http(s) URL")
		}
	}
	return nil
}

func (req *channelRequest) events() []string {
	if req.Events == nil {
		return []string{}
	}
	return req.Events
}

// ListChannels returns all notification channels (without their secret targets).
// @Summary      List notification channels
// @Tags         Notifications
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/channels [get]
func (h *NotificationHandlers) ListChannels() gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := h.repo.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list channels"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"channels": items})
	}
}

// CreateChannel registers a notification channel, encrypting its target URL.
// @Summary      Create notification channel
// @Tags         Notifications
// @Accept       json
// @Produce      json
// @Success      201  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/channels [post]
func (h *NotificationHandlers) CreateChannel() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req channelRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and type are required"})
			return
		}
		if err := req.validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Target == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target URL is required"})
			return
		}
		if !crypto.Available() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store target: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
			return
		}
		enc, err := crypto.Encrypt([]byte(req.Target))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt target"})
			return
		}
		enabled := req.Enabled == nil || *req.Enabled
		ch := &repositories.NotificationChannel{
			Name: req.Name, Type: req.Type, EncryptedTarget: enc, Events: req.events(), Enabled: enabled,
		}
		saved, err := h.repo.Create(c.Request.Context(), ch)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create channel"})
			return
		}
		c.JSON(http.StatusCreated, saved)
	}
}

// UpdateChannel replaces a channel. A blank target keeps the existing one.
// @Summary      Update notification channel
// @Tags         Notifications
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/channels/{id} [put]
func (h *NotificationHandlers) UpdateChannel() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req channelRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and type are required"})
			return
		}
		if err := req.validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var enc []byte
		if req.Target != "" {
			if !crypto.Available() {
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store target: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
				return
			}
			var encErr error
			if enc, encErr = crypto.Encrypt([]byte(req.Target)); encErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt target"})
				return
			}
		}
		enabled := req.Enabled == nil || *req.Enabled
		updated, err := h.repo.Update(c.Request.Context(), c.Param("id"), req.Name, req.Type, req.events(), enabled, enc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update channel"})
			return
		}
		if updated == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}
		c.JSON(http.StatusOK, updated)
	}
}

// DeleteChannel removes a channel.
func (h *NotificationHandlers) DeleteChannel() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h.repo.Delete(c.Request.Context(), c.Param("id")); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete channel"})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// TestChannel sends a test notification through a channel.
// @Summary      Test notification channel
// @Tags         Notifications
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      502  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/channels/{id}/test [post]
func (h *NotificationHandlers) TestChannel() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h.notifier.SendTest(c.Request.Context(), c.Param("id")); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "sent"})
	}
}
