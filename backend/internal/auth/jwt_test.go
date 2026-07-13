package auth

import (
	"os"
	"strings"
	"testing"
	"time"

	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
)

// A fixed 64-char hex secret so the sync.Once TokenManager construction is
// deterministic across the whole test binary.
const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) {
	// Must be set before the first ValidateJWTSecret call — the secret is
	// resolved once per process.
	os.Setenv("TSM_JWT_SECRET", testSecret)
	os.Exit(m.Run())
}

func TestIsDevMode(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"TRUE", false}, // explicit lowercase contract
	}
	for _, tt := range tests {
		t.Run("DEV_MODE="+tt.value, func(t *testing.T) {
			t.Setenv("DEV_MODE", tt.value)
			if got := IsDevMode(); got != tt.want {
				t.Errorf("IsDevMode() with DEV_MODE=%q = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestGenerateRandomSecret(t *testing.T) {
	a, err := generateRandomSecret()
	if err != nil {
		t.Fatalf("generateRandomSecret() error: %v", err)
	}
	if len(a) != 64 {
		t.Errorf("expected 64 hex chars (32 bytes), got %d", len(a))
	}
	b, err := generateRandomSecret()
	if err != nil {
		t.Fatalf("generateRandomSecret() second call error: %v", err)
	}
	if a == b {
		t.Error("two generated secrets are identical — not random")
	}
}

func TestValidateJWTSecret(t *testing.T) {
	if err := ValidateJWTSecret(); err != nil {
		t.Fatalf("ValidateJWTSecret() with TSM_JWT_SECRET set returned error: %v", err)
	}
	// Idempotent: subsequent calls return the same (nil) result.
	if err := ValidateJWTSecret(); err != nil {
		t.Fatalf("second ValidateJWTSecret() returned error: %v", err)
	}
}

func TestGenerateAndValidateJWT(t *testing.T) {
	scopes := []string{"state:read", "sources:manage"}
	token, err := GenerateJWT("user-123", "user@example.com", scopes, time.Hour)
	if err != nil {
		t.Fatalf("GenerateJWT() error: %v", err)
	}
	if token == "" {
		t.Fatal("GenerateJWT() returned an empty token")
	}

	claims, err := ValidateJWT(token)
	if err != nil {
		t.Fatalf("ValidateJWT() error: %v", err)
	}
	if claims.UserID != "user-123" {
		t.Errorf("UserID = %q, want user-123", claims.UserID)
	}
	if claims.Email != "user@example.com" {
		t.Errorf("Email = %q, want user@example.com", claims.Email)
	}
	if len(claims.Scopes) != 2 || claims.Scopes[0] != "state:read" || claims.Scopes[1] != "sources:manage" {
		t.Errorf("Scopes = %v, want %v", claims.Scopes, scopes)
	}
	if claims.JTI == "" {
		t.Error("JTI is empty — revocation requires a token ID")
	}
	if claims.Issuer != jwtIssuer {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, jwtIssuer)
	}
}

func TestValidateJWT_Expired(t *testing.T) {
	token, err := GenerateJWT("user-123", "user@example.com", nil, -time.Minute)
	if err != nil {
		t.Fatalf("GenerateJWT() error: %v", err)
	}
	if _, err := ValidateJWT(token); err == nil {
		t.Error("ValidateJWT() accepted an expired token")
	}
}

func TestValidateJWT_Tampered(t *testing.T) {
	token, err := GenerateJWT("user-123", "user@example.com", nil, time.Hour)
	if err != nil {
		t.Fatalf("GenerateJWT() error: %v", err)
	}

	// Corrupt the signature segment.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT segments, got %d", len(parts))
	}
	sig := []byte(parts[2])
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)

	if _, err := ValidateJWT(tampered); err == nil {
		t.Error("ValidateJWT() accepted a token with a corrupted signature")
	}
}

func TestValidateJWT_Garbage(t *testing.T) {
	for _, tok := range []string{"", "not-a-jwt", "a.b", "a.b.c.d"} {
		if _, err := ValidateJWT(tok); err == nil {
			t.Errorf("ValidateJWT(%q) did not return an error", tok)
		}
	}
}

// ---------------------------------------------------------------------------
// Issuer pin (issue #178 / terraform-suite-identity audit #51)
// ---------------------------------------------------------------------------

func TestValidateJWT_TrustedSiblingIssuerAccepted(t *testing.T) {
	if err := ValidateJWTSecret(); err != nil {
		t.Fatalf("ValidateJWTSecret() error: %v", err)
	}
	// A token signed with the SAME secret but terraform-registry-backend's own
	// issuer — simulating a sibling app in the shared-secret coupled suite. The
	// sibling also stamps this app's own identity as the audience, as a
	// well-behaved sibling adopting the coupled-suite convention would when
	// minting a token meant to be honored here — see
	// TestValidateJWT_AudienceEnforced below for the cases where it does not
	// (or audiences it for itself instead).
	sibling := idauth.NewTokenManager(testSecret, siblingIssuer)
	sibling.SetAudience(jwtIssuer)
	token, err := sibling.Generate("user-1", "user1@example.com", nil, time.Hour)
	if err != nil {
		t.Fatalf("sibling Generate() error: %v", err)
	}
	claims, err := ValidateJWT(token)
	if err != nil {
		t.Fatalf("expected the trusted sibling's token to validate: %v", err)
	}
	if claims.UserID != "user-1" {
		t.Errorf("UserID = %q, want user-1", claims.UserID)
	}
}

func TestValidateJWT_UntrustedIssuerRejected(t *testing.T) {
	if err := ValidateJWTSecret(); err != nil {
		t.Fatalf("ValidateJWTSecret() error: %v", err)
	}
	// A token signed with the SAME secret but an issuer that is neither this
	// app nor the trusted sibling must be rejected, even though the signature
	// itself is valid — this is exactly the gap issue #178 closes.
	untrusted := idauth.NewTokenManager(testSecret, "some-other-app")
	token, err := untrusted.Generate("user-2", "user2@example.com", nil, time.Hour)
	if err != nil {
		t.Fatalf("untrusted Generate() error: %v", err)
	}
	if _, err := ValidateJWT(token); err == nil {
		t.Error("expected an error validating a token from an untrusted issuer")
	}
}

// ---------------------------------------------------------------------------
// Audience enforcement (issue #178, completed via the terraform-suite-identity
// v0.17.0 adoption)
// ---------------------------------------------------------------------------

// TestValidateJWT_AudienceEnforced confirms ValidateJWTSecret now also stamps
// and requires this app's own audience, closing the half of #178 that the
// issuer pin alone left open: a trusted sibling issuer is no longer
// sufficient on its own — the token must also have been minted FOR this app.
func TestValidateJWT_AudienceEnforced(t *testing.T) {
	if err := ValidateJWTSecret(); err != nil {
		t.Fatalf("ValidateJWTSecret() error: %v", err)
	}

	t.Run("own token carries own audience and validates", func(t *testing.T) {
		token, err := GenerateJWT("user-1", "user1@example.com", nil, time.Hour)
		if err != nil {
			t.Fatalf("GenerateJWT: %v", err)
		}
		if _, err := ValidateJWT(token); err != nil {
			t.Errorf("own token with own audience should validate: %v", err)
		}
	})

	t.Run("trusted issuer but no audience claim is rejected", func(t *testing.T) {
		// The sibling's issuer is trusted, but this token never had an audience
		// stamped — simulating a sibling that has not (yet) adopted SetAudience.
		// Trusting the issuer must not be enough on its own.
		sibling := idauth.NewTokenManager(testSecret, siblingIssuer)
		token, err := sibling.Generate("attacker-or-unaware-sibling", "x@example.com", []string{"admin"}, time.Hour)
		if err != nil {
			t.Fatalf("sibling Generate: %v", err)
		}
		if _, err := ValidateJWT(token); err == nil {
			t.Error("a trusted-issuer token with no audience claim must be rejected once this app requires an audience")
		}
	})

	t.Run("trusted issuer but wrong audience is rejected", func(t *testing.T) {
		// The sibling stamps ITS OWN identity as the audience (i.e. mints a
		// normal token for its own use), not this app's — this must not validate
		// here even though the issuer is trusted and the secret matches.
		sibling := idauth.NewTokenManager(testSecret, siblingIssuer)
		sibling.SetAudience(siblingIssuer)
		token, err := sibling.Generate("user-1", "user1@example.com", nil, time.Hour)
		if err != nil {
			t.Fatalf("sibling Generate: %v", err)
		}
		if _, err := ValidateJWT(token); err == nil {
			t.Error("a trusted-issuer token audienced for a different app must be rejected")
		}
	})

	t.Run("trusted issuer and correct audience validates", func(t *testing.T) {
		sibling := idauth.NewTokenManager(testSecret, siblingIssuer)
		sibling.SetAudience(jwtIssuer)
		token, err := sibling.Generate("user-1", "user1@example.com", nil, time.Hour)
		if err != nil {
			t.Fatalf("sibling Generate: %v", err)
		}
		if _, err := ValidateJWT(token); err != nil {
			t.Errorf("a trusted-issuer token correctly audienced for this app should validate: %v", err)
		}
	})
}
