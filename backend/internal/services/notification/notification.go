// Package notification implements the notification dispatcher that sends messages
// through configured notification channels (webhook, Slack, Teams, email).
package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Service dispatches notifications through configured channels.
type Service struct {
	channelRepo *repositories.NotificationChannelRepository
}

// NewService creates a new notification Service.
func NewService(channelRepo *repositories.NotificationChannelRepository) *Service {
	return &Service{
		channelRepo: channelRepo,
	}
}

// Send delivers a notification through the specified channel.
func (s *Service) Send(ctx context.Context, channelID, title, message string) error {
	channel, err := s.channelRepo.GetByID(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to load notification channel: %w", err)
	}
	if channel == nil {
		return fmt.Errorf("notification channel not found: %s", channelID)
	}
	if !channel.IsActive {
		slog.Warn("Skipping notification for inactive channel",
			"channel_id", channelID, "channel_name", channel.Name)
		return nil
	}

	return s.dispatch(ctx, channel, title, message)
}

// SendTest sends a test notification through the specified channel.
func (s *Service) SendTest(ctx context.Context, channel *models.NotificationChannel) error {
	return s.dispatch(ctx, channel, "Test Notification", "This is a test notification from Terraform State Manager.")
}

// dispatch routes the notification to the appropriate sender based on channel type.
func (s *Service) dispatch(ctx context.Context, channel *models.NotificationChannel, title, message string) error {
	switch channel.ChannelType {
	case models.ChannelTypeWebhook:
		return s.dispatchWebhook(ctx, channel.Config, title, message)
	case models.ChannelTypeSlack:
		return s.dispatchSlack(ctx, channel.Config, title, message)
	case models.ChannelTypeTeams:
		return s.dispatchTeams(ctx, channel.Config, title, message)
	case models.ChannelTypeEmail:
		return s.dispatchEmail(ctx, channel.Config, title, message)
	default:
		return fmt.Errorf("unsupported channel type: %s", channel.ChannelType)
	}
}

// dispatchWebhook extracts the URL from channel config and sends a webhook.
func (s *Service) dispatchWebhook(ctx context.Context, config json.RawMessage, title, message string) error {
	var cfg struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("failed to parse webhook config: %w", err)
	}
	if cfg.URL == "" {
		return fmt.Errorf("webhook URL is required")
	}

	payload := map[string]interface{}{
		"title":   title,
		"message": message,
	}
	return SendWebhook(ctx, cfg.URL, payload)
}

// dispatchSlack extracts the webhook URL from channel config and sends a Slack message.
func (s *Service) dispatchSlack(ctx context.Context, config json.RawMessage, title, message string) error {
	var cfg struct {
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("failed to parse slack config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return fmt.Errorf("slack webhook URL is required")
	}

	return SendSlack(ctx, cfg.WebhookURL, title, message)
}

// dispatchTeams extracts the webhook URL from channel config and sends a Teams message.
func (s *Service) dispatchTeams(ctx context.Context, config json.RawMessage, title, message string) error {
	var cfg struct {
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("failed to parse teams config: %w", err)
	}
	if cfg.WebhookURL == "" {
		return fmt.Errorf("teams webhook URL is required")
	}

	return SendTeams(ctx, cfg.WebhookURL, title, message)
}

// dispatchEmail extracts SMTP config and sends an email notification.
func (s *Service) dispatchEmail(ctx context.Context, config json.RawMessage, title, message string) error {
	var cfg struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		From     string `json:"from"`
		To       string `json:"to"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return fmt.Errorf("failed to parse email config: %w", err)
	}

	emailCfg := EmailConfig{
		Host:     cfg.Host,
		Port:     cfg.Port,
		Username: cfg.Username,
		Password: cfg.Password,
		From:     cfg.From,
	}

	return SendEmail(ctx, emailCfg, cfg.To, title, message)
}
