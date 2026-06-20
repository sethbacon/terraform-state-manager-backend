// Package notify delivers alert events to configured notification channels
// (generic webhook, Slack/Teams incoming-webhook, or email via a shared SMTP
// relay). It is a leaf service: it depends on the channel repository and crypto,
// never on the HTTP layer. Channel targets are stored encrypted and decrypted
// only here at send time — a destination URL for the webhook types, or the
// recipient address(es) for the email type.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
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

// SMTPConfig is the shared outbound mail relay backing email channels. Host empty
// disables the email channel type. Mirrors config.SMTPConfig so this package stays
// independent of the config package.
type SMTPConfig struct {
	Host     string
	Port     int
	From     string
	Username string
	Password string
}

// smtpSender sends a pre-built RFC 5322 message to the recipients through the
// relay. It is a field on Notifier so tests can stub the transport.
type smtpSender func(ctx context.Context, cfg SMTPConfig, to []string, msg []byte) error

// Notifier fans an Event out to the channels subscribed to it.
type Notifier struct {
	repo   *repositories.NotificationChannelRepository
	client *http.Client
	smtp   SMTPConfig
	mailer smtpSender
	logger *slog.Logger
}

// New builds a Notifier over the channel repository. smtp configures the shared
// relay for email channels (an empty Host disables the email type).
func New(repo *repositories.NotificationChannelRepository, smtp SMTPConfig) *Notifier {
	return &Notifier{
		repo:   repo,
		client: &http.Client{Timeout: 10 * time.Second},
		smtp:   smtp,
		mailer: sendSMTP,
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
	target, err := n.decryptTarget(ch)
	if err != nil {
		n.record(ctx, ch.ID, err)
		return err
	}
	// Email targets are recipient address(es) sent through the shared relay; the
	// other types POST to the decrypted destination URL.
	var sendErr error
	if ch.Type == "email" {
		sendErr = n.sendEmail(ctx, target, title, message)
	} else {
		sendErr = n.send(ctx, ch.Type, target, title, message)
	}
	if sendErr != nil {
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
	case "teams":
		// Microsoft Teams via a Power Automate "Workflows" incoming webhook, which
		// expects an Adaptive Card message envelope (the classic Office 365
		// connector MessageCard format is being retired).
		payload = teamsPayload(title, message)
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

// teamsPayload builds the Adaptive Card message envelope a Teams "Workflows"
// incoming webhook accepts: a single text card with a bold title over the body.
func teamsPayload(title, message string) map[string]any {
	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"type":    "AdaptiveCard",
				"version": "1.4",
				"body": []map[string]any{
					{"type": "TextBlock", "text": title, "weight": "Bolder", "size": "Medium", "wrap": true},
					{"type": "TextBlock", "text": message, "wrap": true},
				},
			},
		}},
	}
}

// sendEmail delivers the alert to the channel's recipient(s) through the shared
// SMTP relay. recipients is the decrypted, comma-separated address list stored as
// the channel target.
func (n *Notifier) sendEmail(ctx context.Context, recipients, title, message string) error {
	if n.smtp.Host == "" {
		return fmt.Errorf("email channel requires an SMTP relay (set TSM_NOTIFICATIONS_SMTP_HOST)")
	}
	if n.smtp.From == "" {
		return fmt.Errorf("email channel requires a from address (set TSM_NOTIFICATIONS_SMTP_FROM)")
	}
	to, err := ParseRecipients(recipients)
	if err != nil {
		return err
	}
	return n.mailer(ctx, n.smtp, to, buildEmailMessage(n.smtp.From, to, title, message))
}

// ParseRecipients splits a comma-separated recipient list and validates each as
// an RFC 5322 address. Shared by the API (channel validation) and the email
// sender so both agree on what a valid target looks like.
func ParseRecipients(list string) ([]string, error) {
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		addr := strings.TrimSpace(p)
		if addr == "" {
			continue
		}
		if _, err := mail.ParseAddress(addr); err != nil {
			return nil, fmt.Errorf("invalid email address %q", addr)
		}
		out = append(out, addr)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one recipient email is required")
	}
	return out, nil
}

// buildEmailMessage renders a minimal text/plain RFC 5322 message. The subject is
// stripped of CR/LF to defeat header injection (the body is internal alert text).
func buildEmailMessage(from string, to []string, title, message string) []byte {
	subject := strings.NewReplacer("\r", " ", "\n", " ").Replace(title)
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(message)
	b.WriteString("\r\n")
	return b.Bytes()
}

// sendSMTP is the live transport: dial the relay, opportunistically upgrade to TLS
// (STARTTLS), authenticate when credentials are configured, and send the message.
func sendSMTP(ctx context.Context, cfg SMTPConfig, to []string, msg []byte) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial smtp relay: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer func() { _ = c.Close() }()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if cfg.Username != "" {
		// PlainAuth refuses to send the password over an unencrypted connection,
		// so this fails closed if the relay offered no STARTTLS above.
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp rcpt %q: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp finalize: %w", err)
	}
	return c.Quit()
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
