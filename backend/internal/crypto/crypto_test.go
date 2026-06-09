package crypto

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Setenv("TSM_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef") // 32 raw bytes

	plaintext := []byte(`{"token":"super-secret"}`)
	ct, err := Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}
	got, err := Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip mismatch: got %q", got)
	}
}

func TestHexKey(t *testing.T) {
	// 64 hex chars = 32 bytes.
	t.Setenv("TSM_ENCRYPTION_KEY", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if !Available() {
		t.Fatal("expected key to be available")
	}
	if _, err := Encrypt([]byte("x")); err != nil {
		t.Fatalf("Encrypt with hex key: %v", err)
	}
}

func TestNoKey(t *testing.T) {
	t.Setenv("TSM_ENCRYPTION_KEY", "")
	t.Setenv("ENCRYPTION_KEY", "")
	if Available() {
		t.Fatal("expected no key")
	}
	if _, err := Encrypt([]byte("x")); err == nil {
		t.Error("expected error without key")
	}
}
