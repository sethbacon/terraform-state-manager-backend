package notify

import (
	"testing"

	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
)

func TestEventConstants(t *testing.T) {
	// These two strings are persisted (notification_channels.events) and must
	// never change without a migration.
	if EventDriftDetected != "drift_detected" {
		t.Errorf("EventDriftDetected = %q, want %q", EventDriftDetected, "drift_detected")
	}
	if EventRunFailed != "run_failed" {
		t.Errorf("EventRunFailed = %q, want %q", EventRunFailed, "run_failed")
	}
}

func TestNew_NilSMTPDoesNotPanic(t *testing.T) {
	tc, err := identitycrypto.NewTokenCipher(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewTokenCipher: %v", err)
	}
	n := New(nil, nil, tc, nil)
	if n == nil {
		t.Fatal("New returned nil")
	}
}
