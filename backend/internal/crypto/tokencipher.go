// Package crypto provides AES-256-GCM authenticated encryption for sensitive
// values stored at rest in the database, such as state source credentials.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"

	"golang.org/x/crypto/pbkdf2"
)

var (
	ErrKeyLengthInvalid    = errors.New("crypto: key must be exactly 32 bytes for AES-256")
	ErrCiphertextCorrupted = errors.New("crypto: ciphertext is corrupted or tampered")
	ErrDecryptionFailed    = errors.New("crypto: decryption operation failed")
	ErrSaltTooShort        = errors.New("crypto: salt must be at least 16 bytes")
)

// TokenCipher encrypts and decrypts sensitive token data
type TokenCipher struct {
	masterKey []byte
}

// NewTokenCipher creates a cipher with a 32-byte master key
func NewTokenCipher(masterKey []byte) (*TokenCipher, error) {
	if len(masterKey) != 32 {
		return nil, ErrKeyLengthInvalid
	}
	keyCopy := make([]byte, 32)
	copy(keyCopy, masterKey)
	return &TokenCipher{masterKey: keyCopy}, nil
}

// DeriveTokenCipher creates a cipher by deriving a key from a passphrase
func DeriveTokenCipher(passphrase string, salt []byte, iterations int) (*TokenCipher, error) {
	if len(salt) < 16 {
		return nil, ErrSaltTooShort
	}
	if iterations < 10000 {
		iterations = 100000
	}
	derivedKey := pbkdf2.Key([]byte(passphrase), salt, iterations, 32, sha256.New)
	return NewTokenCipher(derivedKey)
}

// Seal encrypts plaintext and returns a base64-encoded ciphertext
func (tc *TokenCipher) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	blockCipher, err := aes.NewCipher(tc.masterKey)
	if err != nil {
		return "", err
	}

	aead, err := cipher.NewGCM(blockCipher)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.URLEncoding.EncodeToString(sealed), nil
}

// Open decrypts a base64-encoded ciphertext and returns the plaintext
func (tc *TokenCipher) Open(encodedCiphertext string) (string, error) {
	if encodedCiphertext == "" {
		return "", nil
	}

	ciphertext, err := base64.URLEncoding.DecodeString(encodedCiphertext)
	if err != nil {
		return "", ErrCiphertextCorrupted
	}

	blockCipher, err := aes.NewCipher(tc.masterKey)
	if err != nil {
		return "", err
	}

	aead, err := cipher.NewGCM(blockCipher)
	if err != nil {
		return "", err
	}

	nonceLen := aead.NonceSize()
	if len(ciphertext) < nonceLen {
		return "", ErrCiphertextCorrupted
	}

	nonce := ciphertext[:nonceLen]
	actualCiphertext := ciphertext[nonceLen:]

	plaintext, err := aead.Open(nil, nonce, actualCiphertext, nil)
	if err != nil {
		return "", ErrDecryptionFailed
	}

	return string(plaintext), nil
}

// GenerateKey creates a cryptographically secure random 32-byte key
func GenerateKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	return key, nil
}

// GenerateSalt creates a cryptographically secure random salt
func GenerateSalt(length int) ([]byte, error) {
	if length < 16 {
		length = 16
	}
	salt := make([]byte, length)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return salt, nil
}
