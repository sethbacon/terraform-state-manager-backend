package api

import (
	"encoding/hex"
	"fmt"
	"os"

	identitycrypto "github.com/sethbacon/terraform-suite-identity/identity/crypto"
)

// buildIdentityTokenCipher constructs the shared identity/crypto.TokenCipher
// used to encrypt/decrypt notification-channel targets. It reads the same
// TSM_ENCRYPTION_KEY / ENCRYPTION_KEY (and optional
// TSM_ENCRYPTION_KEY_PREVIOUS / ENCRYPTION_KEY_PREVIOUS, for zero-downtime key
// rotation) env vars this repo's own internal/crypto package uses, parsed the
// same way (32 raw bytes, or 64 hex characters) — so operators who already
// configured a hex-encoded key for CI-source/OIDC-secret encryption don't need
// a second, differently-formatted key just for notification channels.
//
// Returns an error (rather than panicking or log.Fatal) when no key is
// configured: the notifications feature overall (background jobs, the
// channel-webhook system) is optional, so a deployment with no encryption key
// configured should simply run with that ONE feature unavailable, not fail to
// start entirely.
// BuildIdentityTokenCipher is the exported form, for callers outside this
// package — currently the bind-targets maintenance command. It deliberately
// delegates rather than re-deriving: the backfill must use the SAME key material
// and the SAME parsing as the server, or it cannot open what the server sealed,
// and a second parser is a second thing to drift (see registry-backend #835 for
// how that ends).
func BuildIdentityTokenCipher() (*identitycrypto.TokenCipher, error) {
	return buildIdentityTokenCipher()
}

// CurrentTSMEncryptionKey returns the raw TSM_ENCRYPTION_KEY bytes — the key
// everything is encrypted WITH, without the previous-key decryption fallback.
//
// The rekey-targets maintenance command needs the current key on its own,
// because a cipher that falls back to the previous key cannot answer the only
// question that ends a rotation: is this row still encrypted under the key I am
// about to delete? An open that succeeds through the fallback looks identical to
// one that did not need it (#364).
//
// It reads the key here, beside the cipher constructor and through the same
// parser, rather than in the command: one key source, so the sweep and the
// server cannot disagree about what "the current key" is.
func CurrentTSMEncryptionKey() ([]byte, error) {
	key, err := parseTSMEncryptionKey("TSM_ENCRYPTION_KEY", "ENCRYPTION_KEY")
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, fmt.Errorf("encryption key not configured (set TSM_ENCRYPTION_KEY or ENCRYPTION_KEY)")
	}
	return key, nil
}

func buildIdentityTokenCipher() (*identitycrypto.TokenCipher, error) {
	key, err := parseTSMEncryptionKey("TSM_ENCRYPTION_KEY", "ENCRYPTION_KEY")
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, fmt.Errorf("encryption key not configured (set TSM_ENCRYPTION_KEY or ENCRYPTION_KEY)")
	}
	previous, err := parseTSMEncryptionKey("TSM_ENCRYPTION_KEY_PREVIOUS", "ENCRYPTION_KEY_PREVIOUS")
	if err != nil {
		// A malformed *previous* key must not block startup with the current
		// (valid) key — rotation support is best-effort.
		previous = nil
	}
	if previous != nil {
		return identitycrypto.NewTokenCipherWithPrevious(key, previous)
	}
	return identitycrypto.NewTokenCipher(key)
}

// parseTSMEncryptionKey reads envVar (falling back to fallbackVar) and
// decodes it as either 32 raw bytes or 64 hex characters, mirroring
// internal/crypto.loadKey(). Returns (nil, nil) when neither var is set (not
// an error — the previous-key case is optional).
func parseTSMEncryptionKey(envVar, fallbackVar string) ([]byte, error) {
	v := os.Getenv(envVar)
	if v == "" {
		v = os.Getenv(fallbackVar)
	}
	if v == "" {
		return nil, nil
	}
	if len(v) == 64 {
		if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if len(v) == 32 {
		return []byte(v), nil
	}
	return nil, fmt.Errorf("%s must be 32 raw bytes or 64 hex characters", envVar)
}
