package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewBackendFromRawConfig_LocalDistinctBasePaths verifies that two local
// backends built from distinct source_config / target_config base paths resolve
// to different directories rather than sharing one. This guards against the
// regression where the migration storage factory ignored the per-job config and
// built every backend from the same global StorageConfig.
func TestNewBackendFromRawConfig_LocalDistinctBasePaths(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	mustRaw := func(basePath string) json.RawMessage {
		raw, err := json.Marshal(map[string]string{"base_path": basePath})
		require.NoError(t, err)
		return raw
	}

	src, err := NewBackendFromRawConfig("local", mustRaw(srcDir))
	require.NoError(t, err)
	dst, err := NewBackendFromRawConfig("local", mustRaw(dstDir))
	require.NoError(t, err)

	// Write the same key into each backend with different content.
	require.NoError(t, src.Put(ctx, "state.tfstate", []byte("source")))
	require.NoError(t, dst.Put(ctx, "state.tfstate", []byte("target")))

	// If the two backends shared a directory, the second Put would overwrite
	// the first. Reading each back must return its own content.
	srcData, err := src.Get(ctx, "state.tfstate")
	require.NoError(t, err)
	assert.Equal(t, []byte("source"), srcData)

	dstData, err := dst.Get(ctx, "state.tfstate")
	require.NoError(t, err)
	assert.Equal(t, []byte("target"), dstData)

	// And each file must physically live under its own base directory.
	assert.FileExists(t, filepath.Join(srcDir, "state.tfstate"))
	assert.FileExists(t, filepath.Join(dstDir, "state.tfstate"))
}

// TestNewBackendFromRawConfig_UnsupportedBackend ensures an unknown backend type
// is rejected rather than silently falling through.
func TestNewBackendFromRawConfig_UnsupportedBackend(t *testing.T) {
	b, err := NewBackendFromRawConfig("nope", json.RawMessage(`{}`))
	assert.Error(t, err)
	assert.Nil(t, b)
}
