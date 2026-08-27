// Package crypto provides authenticated encryption (AES-256-GCM) for secrets
// stored at rest — currently the per-source credentials (e.g. an HCP token).
//
// The key is read from TSM_ENCRYPTION_KEY (or the unprefixed ENCRYPTION_KEY, for
// infra that injects a generic secret name): either 32 raw bytes or 64 hex chars.
// Ciphertext layout is nonce || GCM(seal), so it is self-describing.
//
// ROTATION (#368). Decrypt also tries TSM_ENCRYPTION_KEY_PREVIOUS. Without it,
// changing TSM_ENCRYPTION_KEY made every secret written by this package
// unreadable at the NEXT RESTART -- ten call sites covering state-source
// credentials, CI tokens, drift tokens, the OIDC client secret and the SMTP
// password -- because nothing here ever consulted a second key. The previous-key
// variable existed for the sibling TokenCipher family and did nothing for this one.
//
// THIS MAKES A ROTATION SURVIVABLE, NOT COMPLETE. Encrypt always uses the
// CURRENT key, so a secret is re-encrypted only when it is next written. Until
// every row has been rewritten the previous key is still load-bearing, and
// dropping it resurrects the original outage. UsedPreviousKey reports whether
// anything actually needed it, which is the only honest answer to "may I drop
// it yet?" -- see #368 for the sweep that would let a rotation finish.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
)

// ErrNoKey indicates no encryption key is configured.
var ErrNoKey = errors.New("encryption key not configured (set TSM_ENCRYPTION_KEY or ENCRYPTION_KEY to a 32-byte key)")

func loadKey() ([]byte, error) {
	v := os.Getenv("TSM_ENCRYPTION_KEY")
	if v == "" {
		v = os.Getenv("ENCRYPTION_KEY")
	}
	return parseKey(v)
}

// loadPreviousKey returns the retiring key, or nil when none is configured.
//
// Absence is NOT an error: a deployment that has never rotated has no previous
// key, and that is the ordinary case. A previous key that is present but
// MALFORMED is an error, because silently ignoring it would reintroduce exactly
// the failure this exists to prevent -- an operator who believes rotation is
// covered while it is not.
func loadPreviousKey() ([]byte, error) {
	v := os.Getenv("TSM_ENCRYPTION_KEY_PREVIOUS")
	if v == "" {
		return nil, nil
	}
	k, err := parseKey(v)
	if err != nil {
		return nil, fmt.Errorf("TSM_ENCRYPTION_KEY_PREVIOUS: %w", err)
	}
	return k, nil
}

func parseKey(v string) ([]byte, error) {
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
	if pt, err := gcm.Open(nil, nonce, ct, nil); err == nil {
		return pt, nil
	}

	// The current key did not open it. Try the retiring one before failing --
	// during a rotation that is the ordinary case, not an anomaly.
	//
	// A malformed previous key is surfaced rather than swallowed: an operator
	// who set it wrongly must not be told "decryption failed" when the real
	// answer is "your previous key is not a key".
	prev, perr := loadPreviousKey()
	if perr != nil {
		return nil, perr
	}
	if prev == nil {
		return nil, errors.New("crypto: decryption failed and no previous key is configured (set TSM_ENCRYPTION_KEY_PREVIOUS during a rotation)")
	}
	prevGCM, err := gcmFor(prev)
	if err != nil {
		return nil, err
	}
	pt, err := prevGCM.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("crypto: decryption failed under both the current and previous keys")
	}

	// RECORDED, because it is the answer to the only question that matters
	// during a rotation. Encrypt always writes with the CURRENT key, so a row is
	// migrated only when it is next written; until nothing needs the previous
	// key, dropping it recreates the outage. A one-shot log keeps a busy
	// deployment from drowning in it while still making the fact discoverable.
	markPreviousKeyUsed()
	return pt, nil
}

var previousKeyUsed atomic.Bool

func markPreviousKeyUsed() {
	if previousKeyUsed.CompareAndSwap(false, true) {
		slog.Warn("crypto: a secret was decrypted with TSM_ENCRYPTION_KEY_PREVIOUS",
			"impact", "at least one stored secret is still encrypted under the retiring key; dropping TSM_ENCRYPTION_KEY_PREVIOUS now would make it unreadable",
			"remedy", "rewrite the affected secrets (re-save them) until this stops being reported, then drop the previous key")
	}
}

// UsedPreviousKey reports whether any decryption since start-up needed the
// retiring key. False after a full run of the workload is the evidence that
// TSM_ENCRYPTION_KEY_PREVIOUS can be dropped; true means it cannot.
//
// It is deliberately a positive observation rather than a scan: nothing can tell
// which key a ciphertext used without trying, so "nothing needed it" is the only
// answer that can actually be established.
func UsedPreviousKey() bool { return previousKeyUsed.Load() }

func newGCM() (cipher.AEAD, error) {
	key, err := loadKey()
	if err != nil {
		return nil, err
	}
	return gcmFor(key)
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
