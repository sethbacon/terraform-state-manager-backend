package crypto

import (
	"os"
	"strings"
	"testing"
)

// #368 — rotating TSM_ENCRYPTION_KEY used to make every secret this package
// wrote unreadable at the next restart, because nothing here ever consulted a
// second key. Ten call sites depend on it: state-source credentials, CI tokens,
// drift tokens, the OIDC client secret and the SMTP password.

const (
	keyA = "0123456789abcdef0123456789abcdef"
	keyB = "fedcba9876543210fedcba9876543210"
)

func withKeys(t *testing.T, current, previous string) {
	t.Helper()
	t.Setenv("TSM_ENCRYPTION_KEY", current)
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("TSM_ENCRYPTION_KEY_PREVIOUS", previous)
	previousKeyUsed.Store(false)
}

// THE ROTATION ITSELF: a secret sealed under the old key must still open after
// the operator swaps in a new one.
func TestDecrypt_OpensCiphertextSealedUnderThePreviousKey(t *testing.T) {
	withKeys(t, keyA, "")
	sealed, err := Encrypt([]byte("hcp-token"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// The rotation: yesterday's key becomes the previous one.
	withKeys(t, keyB, keyA)

	got, err := Decrypt(sealed)
	if err != nil {
		t.Fatalf("Decrypt after rotation: %v", err)
	}
	if string(got) != "hcp-token" {
		t.Errorf("plaintext = %q, want hcp-token", got)
	}
	if !UsedPreviousKey() {
		t.Error("UsedPreviousKey must report true; it is the only evidence that the previous key cannot yet be dropped")
	}
}

// The falsification. Without it, "always try both keys and report used" would
// satisfy the test above while reporting the previous key as needed forever,
// so an operator could never finish a rotation.
func TestDecrypt_CurrentKeyAloneDoesNotFlagThePreviousKeyAsUsed(t *testing.T) {
	withKeys(t, keyA, keyB)
	sealed, err := Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := Decrypt(sealed); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if UsedPreviousKey() {
		t.Error("the current key opened it; reporting the previous key as used would mean a rotation can never be declared finished")
	}
}

// Encrypt must ALWAYS use the current key, or a rotation would never make
// progress and the previous key could never be retired.
func TestEncrypt_AlwaysUsesTheCurrentKey(t *testing.T) {
	withKeys(t, keyA, keyB)
	sealed, err := Encrypt([]byte("fresh"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Drop the previous key entirely: freshly written ciphertext must not need it.
	withKeys(t, keyA, "")
	if _, err := Decrypt(sealed); err != nil {
		t.Fatalf("ciphertext written after rotation must open under the current key alone: %v", err)
	}
}

// A key neither current nor previous must still fail. The fallback widens what
// opens, and must not widen it to everything.
func TestDecrypt_FailsWhenNeitherKeyMatches(t *testing.T) {
	withKeys(t, keyA, "")
	sealed, _ := Encrypt([]byte("secret"))

	withKeys(t, keyB, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := Decrypt(sealed); err == nil {
		t.Fatal("decryption must fail when neither the current nor the previous key opens the ciphertext")
	}
}

// A MALFORMED previous key is an error, not a silent skip. Swallowing it would
// tell an operator "decryption failed" when the real answer is "your previous
// key is not a key" — reintroducing the false assurance this issue is about.
func TestDecrypt_MalformedPreviousKeyIsReportedNotIgnored(t *testing.T) {
	withKeys(t, keyA, "")
	sealed, _ := Encrypt([]byte("secret"))

	withKeys(t, keyB, "too-short")
	_, err := Decrypt(sealed)
	if err == nil {
		t.Fatal("expected an error")
	}
	// Assert on what is UNIQUE to the malformed case. Checking only for
	// "TSM_ENCRYPTION_KEY_PREVIOUS" passed against a version that swallowed the
	// parse error, because the no-previous-key message names that variable too
	// -- two failure paths sharing one observable string.
	if !strings.Contains(err.Error(), "32 raw bytes") {
		t.Errorf("error must explain that the previous key is malformed, got: %v", err)
	}
	if strings.Contains(err.Error(), "no previous key") {
		t.Errorf("a malformed previous key must not be reported as an ABSENT one; that is the swallow this test exists to catch: %v", err)
	}
}

// No previous key configured is the ordinary state, not an error condition —
// but the failure message should say so, since that is the operator's cue.
func TestDecrypt_NoPreviousKeyConfiguredSaysSo(t *testing.T) {
	withKeys(t, keyA, "")
	sealed, _ := Encrypt([]byte("secret"))

	withKeys(t, keyB, "")
	_, err := Decrypt(sealed)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no previous key") {
		t.Errorf("error should point at the missing previous key, got: %v", err)
	}
}

func TestAvailable_UnaffectedByThePreviousKey(t *testing.T) {
	withKeys(t, keyA, "garbage")
	if !Available() {
		t.Error("Available reports on the CURRENT key; a bad previous key must not make the service look unconfigured")
	}
	_ = os.Getenv("")
}
