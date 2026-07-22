// notifications.go implements CRUD + a test action for notification channels
// (alert destinations). Target URLs are capability-bearing secrets, so they are
// encrypted at rest (like pipeline tokens) and never returned by the API.
package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"

	"github.com/gin-gonic/gin"

	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/services/notify"
)

var validChannelTypes = map[string]bool{"webhook": true, "slack": true, "teams": true, "email": true}
var validEvents = map[string]bool{notify.EventDriftDetected: true, notify.EventRunFailed: true}

// NotificationHandlers serves the notification-channel endpoints.
type NotificationHandlers struct {
	repo     *repositories.NotificationChannelRepository
	notifier *notify.Notifier
	audit    auditor
	// tokenCipher encrypts/decrypts channel targets — must be the same shared
	// identity/crypto cipher instance (same key material) the Notifier uses to
	// decrypt at send time.
	tokenCipher *identitycrypto.TokenCipher

	// settingsRepo and smtp back the shared SMTP relay settings endpoints
	// (GET/PUT /notifications/smtp-config, POST /notifications/test-email).
	// Set via WithSMTPSettings; nil until then, in which case those endpoints
	// report 503 rather than panicking.
	settingsRepo *repositories.SystemSettingsRepository
	smtp         *notify.SMTPConfig

	// notifCfg backs the API-key-expiry settings endpoints (GET/PUT
	// /notifications/api-key-expiry). Set via WithAPIKeyExpirySettings; nil
	// until then, in which case those endpoints report 503.
	notifCfg *config.NotificationsConfig
}

// NewNotificationHandlers builds the handlers over the app connection.
// identityDB (may be nil) carries the shared audit log. tokenCipher encrypts
// channel targets at create/update time (must match the cipher passed to
// notify.New so the Notifier can decrypt them at send time).
func NewNotificationHandlers(database, identityDB *sql.DB, notifier *notify.Notifier, tokenCipher *identitycrypto.TokenCipher) *NotificationHandlers {
	return &NotificationHandlers{
		repo:        repositories.NewNotificationChannelRepository(database),
		notifier:    notifier,
		audit:       newAuditor(identityDB),
		tokenCipher: tokenCipher,
	}
}

// WithSMTPSettings wires in the persisted-settings repository and the shared,
// live SMTP config pointer (the same one passed to notify.New) so the SMTP
// relay settings endpoints can read and update it in place. Returns the
// handler for chaining.
func (h *NotificationHandlers) WithSMTPSettings(settingsRepo *repositories.SystemSettingsRepository, smtp *notify.SMTPConfig) *NotificationHandlers {
	h.settingsRepo = settingsRepo
	h.smtp = smtp
	return h
}

// WithAPIKeyExpirySettings wires in the live notifications config pointer so
// the API-key-expiry settings endpoints can read and update it in place.
// Reuses the settingsRepo set by WithSMTPSettings (call after it in the same
// database-available branch). Returns the handler for chaining.
func (h *NotificationHandlers) WithAPIKeyExpirySettings(notifCfg *config.NotificationsConfig) *NotificationHandlers {
	h.notifCfg = notifCfg
	return h
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
		return fmt.Errorf("type must be one of \"webhook\", \"slack\", \"teams\", \"email\"")
	}
	for _, e := range req.Events {
		if !validEvents[e] {
			return fmt.Errorf("unknown event %q (allowed: drift_detected, run_failed)", e)
		}
	}
	if req.Target != "" {
		if req.Type == "email" {
			// Email targets are recipient address(es), not a URL.
			if _, err := notify.ParseRecipients(req.Target); err != nil {
				return err
			}
		} else {
			u, err := url.Parse(req.Target)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("target must be a valid http(s) URL")
			}
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
			serverError(c, err, "failed to list channels")
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
		if h.tokenCipher == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store target: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
			return
		}
		enc, err := h.tokenCipher.Seal(req.Target)
		if err != nil {
			serverError(c, err, "failed to encrypt target")
			return
		}
		enabled := req.Enabled == nil || *req.Enabled
		ch := &repositories.NotificationChannel{
			Name: req.Name, Type: req.Type, EncryptedTarget: enc, Events: req.events(), Enabled: enabled,
		}
		saved, err := h.repo.Create(c.Request.Context(), ch)
		if err != nil {
			serverError(c, err, "failed to create channel")
			return
		}
		h.audit.write(c, "notification_channel.create", "notification_channel", saved.ID,
			map[string]interface{}{"name": saved.Name, "type": saved.Type})
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
		var enc string
		if req.Target != "" {
			if h.tokenCipher == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store target: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
				return
			}
			var encErr error
			if enc, encErr = h.tokenCipher.Seal(req.Target); encErr != nil {
				serverError(c, encErr, "failed to encrypt target")
				return
			}
		}
		enabled := req.Enabled == nil || *req.Enabled
		updated, err := h.repo.Update(c.Request.Context(), c.Param("id"), req.Name, req.Type, req.events(), enabled, enc)
		if err != nil {
			serverError(c, err, "failed to update channel")
			return
		}
		if updated == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}
		h.audit.write(c, "notification_channel.update", "notification_channel", updated.ID,
			map[string]interface{}{"name": updated.Name})
		c.JSON(http.StatusOK, updated)
	}
}

// DeleteChannel removes a channel.
func (h *NotificationHandlers) DeleteChannel() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if err := h.repo.Delete(c.Request.Context(), id); err != nil {
			serverError(c, err, "failed to delete channel")
			return
		}
		h.audit.write(c, "notification_channel.delete", "notification_channel", id, nil)
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
		h.audit.write(c, "notification_channel.test", "notification_channel", c.Param("id"), nil)
		c.JSON(http.StatusOK, gin.H{"status": "sent"})
	}
}

// ---------------------------------------------------------------------------
// Shared SMTP relay settings (backs every "email" channel). Mirrors
// terraform-registry's admin notifications-config API for parity: the relay
// is configured once here (host/port/credentials/from/use_tls), independent
// of any specific channel.
// ---------------------------------------------------------------------------

// notificationsSMTPConfigDB is the persistence shape stored in
// system_settings.notifications_config. Exported so router.go can reuse it
// when reloading the persisted configuration at startup.
type notificationsSMTPConfigDB struct {
	SMTP struct {
		Host              string `json:"host"`
		Port              int    `json:"port"`
		Username          string `json:"username"`
		From              string `json:"from"`
		UseTLS            bool   `json:"use_tls"`
		PasswordEncrypted string `json:"password_encrypted,omitempty"`
	} `json:"smtp"`
	// Expiry is a pointer so a blob persisted before this feature existed
	// (nil) is distinguishable from one that explicitly saved zero/false
	// values -- mirrors terraform-registry's NotificationsConfigDB.Events
	// pointer for the same reason.
	Expiry *notificationsExpiryConfigDB `json:"expiry,omitempty"`
}

// notificationsExpiryConfigDB is the Expiry section of notificationsSMTPConfigDB.
type notificationsExpiryConfigDB struct {
	APIKeyExpiring     bool `json:"api_key_expiring"`
	WarningDays        int  `json:"warning_days"`
	CheckIntervalHours int  `json:"check_interval_hours"`
}

// NotificationsSMTPResponse is the redacted SMTP relay configuration returned
// by GET/PUT; the password is never included.
type NotificationsSMTPResponse struct {
	Host               string `json:"host"`
	Port               int    `json:"port"`
	Username           string `json:"username"`
	From               string `json:"from"`
	UseTLS             bool   `json:"use_tls"`
	PasswordConfigured bool   `json:"password_configured"`
}

// notificationsSMTPInput is the PUT /notifications/smtp-config request body.
// The password is write-only: send a non-empty value to change it, or omit/
// blank it to preserve the currently stored password.
type notificationsSMTPInput struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	UseTLS   bool   `json:"use_tls"`
}

func (h *NotificationHandlers) smtpResponse(passwordConfigured bool) NotificationsSMTPResponse {
	return NotificationsSMTPResponse{
		Host: h.smtp.Host, Port: h.smtp.Port, Username: h.smtp.Username, From: h.smtp.From,
		UseTLS: h.smtp.UseTLS, PasswordConfigured: passwordConfigured,
	}
}

// GetSMTPConfig returns the current shared SMTP relay configuration (password redacted).
// @Summary      Get SMTP relay configuration
// @Tags         Notifications
// @Produce      json
// @Success      200  {object}  NotificationsSMTPResponse
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/smtp-config [get]
func (h *NotificationHandlers) GetSMTPConfig() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.smtp == nil || h.settingsRepo == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "smtp settings are not available"})
			return
		}
		passwordConfigured := h.smtp.Password != ""
		if raw, err := h.settingsRepo.GetNotificationsConfig(c.Request.Context()); err == nil && raw != nil {
			var dbc notificationsSMTPConfigDB
			if json.Unmarshal(raw, &dbc) == nil && dbc.SMTP.PasswordEncrypted != "" {
				passwordConfigured = true
			}
		}
		c.JSON(http.StatusOK, h.smtpResponse(passwordConfigured))
	}
}

// PutSMTPConfig validates and persists the shared SMTP relay configuration,
// then updates the live config in place so the Notifier's next send observes
// the change immediately — no restart required.
// @Summary      Update SMTP relay configuration
// @Tags         Notifications
// @Accept       json
// @Produce      json
// @Success      200  {object}  NotificationsSMTPResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/smtp-config [put]
func (h *NotificationHandlers) PutSMTPConfig() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.smtp == nil || h.settingsRepo == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "smtp settings are not available"})
			return
		}
		var input notificationsSMTPInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.Port == 0 {
			input.Port = 587
		}
		if input.Port < 1 || input.Port > 65535 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "port must be between 1 and 65535"})
			return
		}
		if input.From != "" {
			if _, err := mail.ParseAddress(input.From); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "from must be a valid email address"})
				return
			}
		}

		ctx := c.Request.Context()
		var dbc notificationsSMTPConfigDB
		if raw, err := h.settingsRepo.GetNotificationsConfig(ctx); err == nil && raw != nil {
			_ = json.Unmarshal(raw, &dbc) // preserve the Expiry section; only SMTP fields are mutated below
		}
		existingEncrypted := dbc.SMTP.PasswordEncrypted

		dbc.SMTP.Host = input.Host
		dbc.SMTP.Port = input.Port
		dbc.SMTP.Username = input.Username
		dbc.SMTP.From = input.From
		dbc.SMTP.UseTLS = input.UseTLS
		if input.Password != "" {
			if !crypto.Available() {
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot store password: encryption key not configured (set TSM_ENCRYPTION_KEY)"})
				return
			}
			enc, err := crypto.Encrypt([]byte(input.Password))
			if err != nil {
				serverError(c, err, "failed to encrypt smtp password")
				return
			}
			dbc.SMTP.PasswordEncrypted = string(enc)
		} else {
			dbc.SMTP.PasswordEncrypted = existingEncrypted
		}

		configJSON, err := json.Marshal(dbc)
		if err != nil {
			serverError(c, err, "failed to marshal smtp configuration")
			return
		}
		if err := h.settingsRepo.SetNotificationsConfig(ctx, configJSON); err != nil {
			serverError(c, err, "failed to save smtp configuration")
			return
		}

		// Update the live config in place (never reassign h.smtp) so the
		// Notifier's next send observes the new settings immediately.
		h.smtp.Host = input.Host
		h.smtp.Port = input.Port
		h.smtp.Username = input.Username
		h.smtp.From = input.From
		h.smtp.UseTLS = input.UseTLS
		if input.Password != "" {
			h.smtp.Password = input.Password
		}

		h.audit.write(c, "notifications.smtp_config.update", "notifications", "smtp", nil)

		passwordConfigured := dbc.SMTP.PasswordEncrypted != "" || h.smtp.Password != ""
		c.JSON(http.StatusOK, h.smtpResponse(passwordConfigured))
	}
}

// NotificationsAPIKeyExpiryResponse is the current API-key-expiry
// notification settings. Enabled reflects the master notifications.enabled
// switch (read-only here -- this app has no endpoint to toggle that switch).
type NotificationsAPIKeyExpiryResponse struct {
	Enabled            bool `json:"enabled"`
	APIKeyExpiring     bool `json:"api_key_expiring"`
	WarningDays        int  `json:"api_key_expiry_warning_days"`
	CheckIntervalHours int  `json:"api_key_expiry_check_interval_hours"`
}

// notificationsAPIKeyExpiryInput is the PUT /notifications/api-key-expiry
// request body.
type notificationsAPIKeyExpiryInput struct {
	APIKeyExpiring     bool `json:"api_key_expiring"`
	WarningDays        int  `json:"api_key_expiry_warning_days"`
	CheckIntervalHours int  `json:"api_key_expiry_check_interval_hours"`
}

func (h *NotificationHandlers) apiKeyExpiryResponse() NotificationsAPIKeyExpiryResponse {
	return NotificationsAPIKeyExpiryResponse{
		Enabled:            h.notifCfg.Enabled,
		APIKeyExpiring:     h.notifCfg.Events.APIKeyExpiring,
		WarningDays:        h.notifCfg.APIKeyExpiryWarningDays,
		CheckIntervalHours: h.notifCfg.APIKeyExpiryCheckIntervalHours,
	}
}

// GetAPIKeyExpiryConfig returns the current API-key-expiry notification settings.
// @Summary      Get API key expiry notification configuration
// @Tags         Notifications
// @Produce      json
// @Success      200  {object}  NotificationsAPIKeyExpiryResponse
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/api-key-expiry [get]
func (h *NotificationHandlers) GetAPIKeyExpiryConfig() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.notifCfg == nil || h.settingsRepo == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api key expiry settings are not available"})
			return
		}
		c.JSON(http.StatusOK, h.apiKeyExpiryResponse())
	}
}

// PutAPIKeyExpiryConfig validates and persists the API-key-expiry
// notification settings, then updates the live config in place so the
// background notifier observes enabled/warning-days changes on its next
// tick. CheckIntervalHours only takes effect after a process restart -- the
// notifier's ticker interval is sized once, at construction.
// @Summary      Update API key expiry notification configuration
// @Tags         Notifications
// @Accept       json
// @Produce      json
// @Param        body  body  notificationsAPIKeyExpiryInput  true  "API key expiry settings"
// @Success      200  {object}  NotificationsAPIKeyExpiryResponse
// @Failure      400  {object}  map[string]interface{}
// @Failure      503  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/api-key-expiry [put]
func (h *NotificationHandlers) PutAPIKeyExpiryConfig() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.notifCfg == nil || h.settingsRepo == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api key expiry settings are not available"})
			return
		}
		var input notificationsAPIKeyExpiryInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.WarningDays < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "api_key_expiry_warning_days must not be negative"})
			return
		}
		if input.CheckIntervalHours < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "api_key_expiry_check_interval_hours must not be negative"})
			return
		}

		ctx := c.Request.Context()
		var dbc notificationsSMTPConfigDB
		if raw, err := h.settingsRepo.GetNotificationsConfig(ctx); err == nil && raw != nil {
			_ = json.Unmarshal(raw, &dbc) // preserve the SMTP section; only Expiry is mutated below
		}
		dbc.Expiry = &notificationsExpiryConfigDB{
			APIKeyExpiring:     input.APIKeyExpiring,
			WarningDays:        input.WarningDays,
			CheckIntervalHours: input.CheckIntervalHours,
		}

		configJSON, err := json.Marshal(dbc)
		if err != nil {
			serverError(c, err, "failed to marshal api key expiry configuration")
			return
		}
		if err := h.settingsRepo.SetNotificationsConfig(ctx, configJSON); err != nil {
			serverError(c, err, "failed to save api key expiry configuration")
			return
		}

		// Update the live config in place (never reassign h.notifCfg) so the
		// API-key-expiry notifier observes the change on its next tick.
		h.notifCfg.Events.APIKeyExpiring = input.APIKeyExpiring
		h.notifCfg.APIKeyExpiryWarningDays = input.WarningDays
		h.notifCfg.APIKeyExpiryCheckIntervalHours = input.CheckIntervalHours

		h.audit.write(c, "notifications.api_key_expiry.update", "notifications", "api_key_expiry", nil)

		c.JSON(http.StatusOK, h.apiKeyExpiryResponse())
	}
}

// notificationsTestEmailInput is the POST /notifications/test-email request body.
type notificationsTestEmailInput struct {
	Recipients []string `json:"recipients"`
	Subject    string   `json:"subject"`
}

// TestEmail sends a test email using the current SMTP relay configuration,
// independent of any specific channel. Always returns 200 with
// {success,message}, even when the send fails. Mirrors terraform-registry's
// POST /admin/notifications/test for parity.
// @Summary      Send a test email via the shared SMTP relay
// @Tags         Notifications
// @Accept       json
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Security     BearerAuth
// @Security     CookieAuth
// @Router       /notifications/test-email [post]
func (h *NotificationHandlers) TestEmail() gin.HandlerFunc {
	return func(c *gin.Context) {
		var input notificationsTestEmailInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if len(input.Recipients) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "at least one recipient is required"})
			return
		}
		for _, r := range input.Recipients {
			if _, err := mail.ParseAddress(r); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid email address %q", r)})
				return
			}
		}
		if h.smtp == nil || h.smtp.Host == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "smtp host is not configured"})
			return
		}

		subject := input.Subject
		if subject == "" {
			subject = "Terraform State Manager: test notification"
		}
		body := "This is a test notification email sent from the Terraform State Manager admin notifications settings."

		if err := h.notifier.SendTestEmail(c.Request.Context(), input.Recipients, subject, body); err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "failed to send test email: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "test email sent"})
	}
}
