package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateAPIKey_ReturnsNonEmptyValues(t *testing.T) {
	key, hash, displayPrefix, err := GenerateAPIKey("tsm")
	require.NoError(t, err)

	assert.NotEmpty(t, key)
	assert.NotEmpty(t, hash)
	assert.NotEmpty(t, displayPrefix)
}

func TestGenerateAPIKey_KeyHasExpectedPrefix(t *testing.T) {
	key, _, _, err := GenerateAPIKey("tsm")
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(key, "tsm_"), "key should start with 'tsm_', got: %s", key)
}

func TestGenerateAPIKey_DisplayPrefixLength(t *testing.T) {
	key, _, displayPrefix, err := GenerateAPIKey("tsm")
	require.NoError(t, err)

	// displayPrefix should be capped at DisplayPrefixLength characters
	assert.LessOrEqual(t, len(displayPrefix), DisplayPrefixLength)
	// and it must be a prefix of the full key
	assert.True(t, strings.HasPrefix(key, displayPrefix),
		"display prefix %q should be a prefix of the full key %q", displayPrefix, key)
}

func TestGenerateAPIKey_DifferentKeysEachCall(t *testing.T) {
	key1, _, _, err1 := GenerateAPIKey("tsm")
	key2, _, _, err2 := GenerateAPIKey("tsm")
	require.NoError(t, err1)
	require.NoError(t, err2)

	assert.NotEqual(t, key1, key2, "two generated keys should not be equal")
}

func TestValidateAPIKey_CorrectKey(t *testing.T) {
	key, hash, _, err := GenerateAPIKey("tsm")
	require.NoError(t, err)

	valid := ValidateAPIKey(key, hash)
	assert.True(t, valid)
}

func TestValidateAPIKey_WrongKey(t *testing.T) {
	_, hash, _, err := GenerateAPIKey("tsm")
	require.NoError(t, err)

	valid := ValidateAPIKey("tsm_wrongkey", hash)
	assert.False(t, valid)
}

func TestValidateAPIKey_EmptyKey(t *testing.T) {
	_, hash, _, err := GenerateAPIKey("tsm")
	require.NoError(t, err)

	valid := ValidateAPIKey("", hash)
	assert.False(t, valid)
}

func TestExtractAPIKeyFromHeader_ValidBearer(t *testing.T) {
	key, err := ExtractAPIKeyFromHeader("Bearer tsm_abc123xyz")
	require.NoError(t, err)
	assert.Equal(t, "tsm_abc123xyz", key)
}

func TestExtractAPIKeyFromHeader_EmptyHeader(t *testing.T) {
	key, err := ExtractAPIKeyFromHeader("")
	assert.Error(t, err)
	assert.Empty(t, key)
}

func TestExtractAPIKeyFromHeader_MissingBearerPrefix(t *testing.T) {
	key, err := ExtractAPIKeyFromHeader("tsm_abc123xyz")
	assert.Error(t, err)
	assert.Empty(t, key)
}

func TestExtractAPIKeyFromHeader_BearerWithNoKey(t *testing.T) {
	key, err := ExtractAPIKeyFromHeader("Bearer ")
	assert.Error(t, err)
	assert.Empty(t, key)
}

func TestExtractAPIKeyFromHeader_BearerWithWhitespaceOnly(t *testing.T) {
	key, err := ExtractAPIKeyFromHeader("Bearer    ")
	// key after TrimSpace is empty → error
	assert.Error(t, err)
	assert.Empty(t, key)
}

func TestGenerateAPIKey_CustomPrefix(t *testing.T) {
	key, hash, _, err := GenerateAPIKey("myapp")
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(key, "myapp_"))
	assert.True(t, ValidateAPIKey(key, hash))
}
