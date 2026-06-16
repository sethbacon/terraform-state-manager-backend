package setup

import (
	"strings"
	"testing"
)

func TestResolveSetupToken_GeneratesWhenEmpty(t *testing.T) {
	raw, generated, err := ResolveSetupToken("")
	if err != nil || !generated {
		t.Fatalf("expected a generated token, got generated=%v err=%v", generated, err)
	}
	if !strings.HasPrefix(raw, TokenPrefix) || len(raw) < 40 {
		t.Fatalf("weak generated token: %q", raw)
	}
	// Generated tokens must be unique per call.
	other, _, _ := ResolveSetupToken("")
	if raw == other {
		t.Fatal("generated tokens must differ between calls")
	}
}

func TestResolveSetupToken_UsesProvided(t *testing.T) {
	raw, generated, err := ResolveSetupToken("  a-strong-operator-token-123  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated {
		t.Fatal("must not generate when a token is provided")
	}
	if raw != "a-strong-operator-token-123" {
		t.Fatalf("provided token should be trimmed and returned verbatim, got %q", raw)
	}
}

func TestResolveSetupToken_RejectsShortProvided(t *testing.T) {
	if _, _, err := ResolveSetupToken("short"); err == nil {
		t.Fatal("expected an error for a too-short provided token")
	}
}
