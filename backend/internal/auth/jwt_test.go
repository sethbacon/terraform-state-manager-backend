package auth

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetJWTSecret resets the once guard so the secret can be re-initialised
// during tests. Only safe to call from test code.
func resetJWTSecret(secret string) {
	jwtSecretOnce = sync.Once{}
	jwtSecretErr = nil
	jwtSecret = ""
	os.Setenv("TSM_JWT_SECRET", secret)
}

func TestMain(m *testing.M) {
	os.Setenv("TSM_JWT_SECRET", "test-secret-key-that-is-long-enough-for-testing")
	os.Exit(m.Run())
}

func TestGenerateJWT_And_ValidateJWT(t *testing.T) {
	resetJWTSecret("test-secret-key-that-is-long-enough-for-testing")

	token, err := GenerateJWT("user-42", "alice@example.com", []string{"read", "write"}, time.Hour)
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	claims, err := ValidateJWT(token)
	require.NoError(t, err)
	require.NotNil(t, claims)

	assert.Equal(t, "user-42", claims.UserID)
	assert.Equal(t, "alice@example.com", claims.Email)
	assert.Equal(t, []string{"read", "write"}, claims.Scopes)
	assert.Equal(t, "terraform-state-manager", claims.Issuer)
	assert.Equal(t, "user-42", claims.Subject)
}

func TestGenerateJWT_DefaultExpiry(t *testing.T) {
	resetJWTSecret("test-secret-key-that-is-long-enough-for-testing")

	// Passing 0 duration should default to 1 hour
	token, err := GenerateJWT("user-1", "bob@example.com", nil, 0)
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	claims, err := ValidateJWT(token)
	require.NoError(t, err)

	// Expiry should be roughly 1 hour from now
	expiry := claims.ExpiresAt.Time
	sinceNow := time.Until(expiry)
	assert.True(t, sinceNow > 55*time.Minute, "expected expiry ~1h, got %v", sinceNow)
}

func TestGenerateJWT_NoScopes(t *testing.T) {
	resetJWTSecret("test-secret-key-that-is-long-enough-for-testing")

	token, err := GenerateJWT("user-2", "carol@example.com", nil, time.Hour)
	require.NoError(t, err)

	claims, err := ValidateJWT(token)
	require.NoError(t, err)
	assert.Empty(t, claims.Scopes)
}

func TestValidateJWT_ExpiredToken(t *testing.T) {
	resetJWTSecret("test-secret-key-that-is-long-enough-for-testing")

	// Generate a token that is already expired (negative duration = past expiry)
	token, err := GenerateJWT("user-expired", "expired@example.com", nil, -1*time.Second)
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	claims, err := ValidateJWT(token)
	assert.Error(t, err)
	assert.Nil(t, claims)
}

func TestValidateJWT_TamperedToken(t *testing.T) {
	resetJWTSecret("test-secret-key-that-is-long-enough-for-testing")

	token, err := GenerateJWT("user-tamper", "tamper@example.com", nil, time.Hour)
	require.NoError(t, err)

	// Replace the signature segment entirely so it definitely won't verify
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "JWT must have 3 parts")

	tampered := strings.Join([]string{parts[0], parts[1], "invalidsignatureXXXXXXXXXXXXXXXX"}, ".")

	claims, err := ValidateJWT(tampered)
	assert.Error(t, err)
	assert.Nil(t, claims)
}

func TestValidateJWT_EmptyToken(t *testing.T) {
	resetJWTSecret("test-secret-key-that-is-long-enough-for-testing")

	claims, err := ValidateJWT("")
	assert.Error(t, err)
	assert.Nil(t, claims)
}

func TestValidateJWT_GarbageToken(t *testing.T) {
	resetJWTSecret("test-secret-key-that-is-long-enough-for-testing")

	claims, err := ValidateJWT("not.a.jwt")
	assert.Error(t, err)
	assert.Nil(t, claims)
}

func TestValidateJWTSecret_ShortSecretLogsWarning(t *testing.T) {
	// Short secret (< 32 chars) should still work — only logs a warning
	resetJWTSecret("short-secret")
	err := ValidateJWTSecret()
	assert.NoError(t, err)
}
