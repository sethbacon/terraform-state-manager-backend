package notify

import (
	"context"
	"testing"
)

// TestAuthForSMTP covers the auth-selection branch: no username => nil (an
// internal relay may accept unauthenticated mail); username set => PlainAuth.
func TestAuthForSMTP(t *testing.T) {
	if a := authForSMTP(SMTPConfig{Host: "relay"}); a != nil {
		t.Error("no username should yield a nil auth (unauthenticated relay)")
	}
	if a := authForSMTP(SMTPConfig{Host: "relay", Username: "u", Password: "p"}); a == nil {
		t.Error("username set should yield a non-nil PlainAuth")
	}
}

// TestSendTestEmail_Errors covers the guard branches of SendTestEmail/sendEmail
// that do not require a live relay: a nil Notifier and an unconfigured relay.
func TestSendTestEmail_Errors(t *testing.T) {
	var nilN *Notifier
	if err := nilN.SendTestEmail(context.Background(), []string{"ops@example.com"}, "s", "b"); err == nil {
		t.Error("nil notifier should return an error")
	}

	n, _ := newNotifier(t) // built with an empty SMTPConfig{} (no Host)
	if err := n.SendTestEmail(context.Background(), []string{"ops@example.com"}, "s", "b"); err == nil {
		t.Error("expected an error when the SMTP relay host is not configured")
	}

	// Host set but From empty => the second guard in sendEmail fires.
	n.smtp.Host = "relay.example.com"
	if err := n.SendTestEmail(context.Background(), []string{"ops@example.com"}, "s", "b"); err == nil {
		t.Error("expected an error when the SMTP from address is not configured")
	}
}
