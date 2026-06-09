// Package notify delivers alert events to configured notification channels
// (generic webhook or Slack incoming-webhook). It is a leaf service: it depends on
// the channel repository and crypto, never on the HTTP layer. Channel target URLs
// are stored encrypted and decrypted only here at send time.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/crypto"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// Event types worth alerting on.
const (
	EventDriftDetected = "drift_detected"
	EventRunFailed     = "run_failed"
)

// Event is a single alert-worthy occurrence.
type Event struct {
	Type    string
	Title   string
	Message string
}

// Notifier fans an Event out to the channels subscribed to it.
type Notifier struct {
	repo   *repositories.NotificationChannelRepository
	client *http.Client
	logger *slog.Logger
}

// New builds a Notifier over the channel repository.
func New(repo *repositories.NotificationChannelRepository) *Notifier {
	return &Notifier{
		repo:   repo,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: slog.With("component", "notify"),
	}
}

// Notify delivers ev to every enabled channel subscribed to ev.Type. Best-effort:
// a failing channel is logged and recorded but never blocks the others. Safe to
// call in a goroutine; pass a background context with its own deadline.
func (n *Notifier) Notify(ctx context.Context, ev Event) {
	if n == nil {
		return
	}
	channels, err := n.repo.ListEnabledForEvent(ctx, ev.Type)
	if err != nil {
		n.logger.Error("failed to load notification channels", "event", ev.Type, "error", err)
		return
	}
	for i := range channels {
		_ = n.deliver(ctx, &channels[i], ev.Title, ev.Message)
	}
}

// SendTest delivers a fixed test message to one channel (the UI "test" button).
func (n *Notifier) SendTest(ctx context.Context, channelID string) error {
	if n == nil {
		return fmt.Errorf("notifications are not available")
	}
	ch, err := n.repo.GetByID(ctx, channelID)
	if err != nil {
		return err
	}
	if ch == nil {
		return fmt.Errorf("channel not found")
	}
	return n.deliver(ctx, ch, "Test notification", "This is a test from Terraform State Manager.")
}

func (n *Notifier) deliver(ctx context.Context, ch *repositories.NotificationChannel, title, message string) error {
	url, err := n.decryptTarget(ch)
	if err != nil {
		n.record(ctx, ch.ID, err)
		return err
	}
	if sendErr := n.send(ctx, ch.Type, url, title, message); sendErr != nil {
		n.logger.Warn("notification delivery failed", "channel", ch.Name, "error", sendErr)
		n.record(ctx, ch.ID, sendErr)
		return sendErr
	}
	n.record(ctx, ch.ID, nil)
	return nil
}

func (n *Notifier) decryptTarget(ch *repositories.NotificationChannel) (string, error) {
	if len(ch.EncryptedTarget) == 0 {
		return "", fmt.Errorf("channel has no target configured")
	}
	pt, err := crypto.Decrypt(ch.EncryptedTarget)
	if err != nil {
		return "", fmt.Errorf("decrypt channel target: %w", err)
	}
	return string(pt), nil
}

func (n *Notifier) send(ctx context.Context, channelType, url, title, message string) error {
	var payload any
	switch channelType {
	case "slack":
		// Slack incoming-webhook format.
		payload = map[string]string{"text": title + "\n" + message}
	default:
		// Generic JSON webhook.
		payload = map[string]any{"title": title, "message": message, "source": "terraform-state-manager"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	// The URL is an admin-configured channel target (decrypted above), not user input.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("destination returned status %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) record(ctx context.Context, channelID string, sendErr error) {
	status, msg := "sent", ""
	if sendErr != nil {
		status, msg = "failed", sendErr.Error()
	}
	if err := n.repo.RecordDelivery(ctx, channelID, status, msg, time.Now()); err != nil {
		n.logger.Error("failed to record delivery", "channel_id", channelID, "error", err)
	}
}
