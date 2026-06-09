// Package crypto provides authenticated encryption (AES-256-GCM) for secrets
// stored at rest — currently the per-source credentials (e.g. an HCP token).
//
// The key is read from TSM_ENCRYPTION_KEY (or the unprefixed ENCRYPTION_KEY, for
// infra that injects a generic secret name): either 32 raw bytes or 64 hex chars.
// Ciphertext layout is nonce || GCM(seal), so it is self-describing.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNoKey indicates no encryption key is configured.
var ErrNoKey = errors.New("encryption key not configured (set TSM_ENCRYPTION_KEY or ENCRYPTION_KEY to a 32-byte key)")

func loadKey() ([]byte, error) {
	v := os.Getenv("TSM_ENCRYPTION_KEY")
	if v == "" {
		v = os.Getenv("ENCRYPTION_KEY")
	}
	if v == "" {
		return nil, ErrNoKey
	}
	if len(v) == 64 {
		if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if len(v) == 32 {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("encryption key must be 32 raw bytes or 64 hex characters")
}

// Available reports whether a valid encryption key is configured.
func Available() bool {
	_, err := loadKey()
	return err == nil
}

// Encrypt seals plaintext with AES-256-GCM, returning nonce-prefixed ciphertext.
func Encrypt(plaintext []byte) ([]byte, error) {
	gcm, err := newGCM()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens nonce-prefixed ciphertext produced by Encrypt.
func Decrypt(ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM()
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCM() (cipher.AEAD, error) {
	key, err := loadKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
