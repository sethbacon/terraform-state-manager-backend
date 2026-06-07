package migration

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/terraform-state-manager/terraform-state-manager/internal/storage"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage/local"
)

// newLocalBackend creates a local storage backend rooted at a fresh temp dir.
func newLocalBackend(t *testing.T) *local.Backend {
	t.Helper()
	b, err := local.NewBackend(t.TempDir())
	require.NoError(t, err)
	return b
}

func TestSHA256Hex(t *testing.T) {
	// Known SHA-256 of the empty input.
	assert.Equal(t,
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		sha256Hex([]byte{}))
	// Stable and content-dependent.
	assert.Equal(t, sha256Hex([]byte("hello")), sha256Hex([]byte("hello")))
	assert.NotEqual(t, sha256Hex([]byte("hello")), sha256Hex([]byte("world")))
}

func TestTransferFile_NewFile(t *testing.T) {
	ctx := context.Background()
	src := newLocalBackend(t)
	dst := newLocalBackend(t)

	content := []byte(`{"version":4,"serial":1}`)
	require.NoError(t, src.Put(ctx, "ws/terraform.tfstate", content))

	status, err := transferFile(ctx, src, dst, "ws/terraform.tfstate")
	require.NoError(t, err)
	assert.Equal(t, statusMigrated, status)

	// The destination must hold a byte-identical, checksum-verified copy.
	got, err := dst.Get(ctx, "ws/terraform.tfstate")
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

func TestTransferFile_IdenticalTargetSkipped(t *testing.T) {
	ctx := context.Background()
	src := newLocalBackend(t)
	dst := newLocalBackend(t)

	content := []byte("identical-bytes")
	require.NoError(t, src.Put(ctx, "a/state", content))
	require.NoError(t, dst.Put(ctx, "a/state", content))

	status, err := transferFile(ctx, src, dst, "a/state")
	require.NoError(t, err)
	assert.Equal(t, statusSkipped, status, "byte-identical target should be skipped")
}

func TestTransferFile_DivergentTargetReTransferred(t *testing.T) {
	ctx := context.Background()
	src := newLocalBackend(t)
	dst := newLocalBackend(t)

	srcContent := []byte("the-real-source-state")
	require.NoError(t, src.Put(ctx, "a/state", srcContent))
	// Target already has different content — must be overwritten, not skipped.
	require.NoError(t, dst.Put(ctx, "a/state", []byte("stale-divergent-state")))

	status, err := transferFile(ctx, src, dst, "a/state")
	require.NoError(t, err)
	assert.Equal(t, statusMigrated, status, "divergent target should be re-transferred")

	got, err := dst.Get(ctx, "a/state")
	require.NoError(t, err)
	assert.Equal(t, srcContent, got, "target should converge on source content")
}

func TestTransferFile_MissingSource(t *testing.T) {
	ctx := context.Background()
	src := newLocalBackend(t)
	dst := newLocalBackend(t)

	status, err := transferFile(ctx, src, dst, "does-not-exist")
	require.Error(t, err)
	assert.Equal(t, statusFailed, status)
	assert.Contains(t, err.Error(), "failed to download")
}

// corruptingBackend wraps a storage.Backend and silently mutates data on Put,
// simulating a corrupted landing at the target so the read-back checksum fails.
type corruptingBackend struct {
	storage.Backend
}

func (c *corruptingBackend) Put(ctx context.Context, path string, data []byte) error {
	corrupted := append([]byte("CORRUPT:"), data...)
	return c.Backend.Put(ctx, path, corrupted)
}

func TestTransferFile_CorruptedLandingFails(t *testing.T) {
	ctx := context.Background()
	src := newLocalBackend(t)
	dst := &corruptingBackend{Backend: newLocalBackend(t)}

	require.NoError(t, src.Put(ctx, "a/state", []byte("good-state")))

	status, err := transferFile(ctx, src, dst, "a/state")
	require.Error(t, err)
	assert.Equal(t, statusFailed, status)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

// TestExecuteMigration_TwoLocalBackends exercises the full executeMigration loop
// across two independent local backends (two t.TempDir() dirs), confirming every
// file is transferred and checksum-verified at the destination.
func TestExecuteMigration_TwoLocalBackends(t *testing.T) {
	ctx := context.Background()
	src := newLocalBackend(t)
	dst := newLocalBackend(t)

	files := map[string][]byte{
		"prod/terraform.tfstate":    []byte(`{"serial":1,"env":"prod"}`),
		"staging/terraform.tfstate": []byte(`{"serial":7,"env":"staging"}`),
		"dev/terraform.tfstate":     []byte(`{"serial":3,"env":"dev"}`),
	}
	for path, content := range files {
		require.NoError(t, src.Put(ctx, path, content))
	}

	// Transfer every listed file and assert each is verified at the destination.
	// List returns paths in the backend's native separator form, so compare each
	// destination object against the source object read back via the same path
	// rather than against the original map keys.
	listed, err := src.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, listed, len(files))

	for _, path := range listed {
		status, transferErr := transferFile(ctx, src, dst, path)
		require.NoError(t, transferErr, "transfer of %s should succeed", path)
		assert.Equal(t, statusMigrated, status)

		srcBytes, srcErr := src.Get(ctx, path)
		require.NoError(t, srcErr)
		dstBytes, dstErr := dst.Get(ctx, path)
		require.NoError(t, dstErr)
		assert.Equal(t, sha256Hex(srcBytes), sha256Hex(dstBytes),
			"destination checksum must match source for %s", path)
	}
}
