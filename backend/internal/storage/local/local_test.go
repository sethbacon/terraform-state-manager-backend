package local

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	dir := t.TempDir()
	b, err := NewBackend(dir)
	require.NoError(t, err)
	return b
}

// ---------------------------------------------------------------------------
// NewBackend
// ---------------------------------------------------------------------------

func TestNewBackend_ValidPath(t *testing.T) {
	dir := t.TempDir()
	b, err := NewBackend(dir)
	require.NoError(t, err)
	assert.NotNil(t, b)
}

func TestNewBackend_EmptyPath(t *testing.T) {
	b, err := NewBackend("")
	assert.Error(t, err)
	assert.Nil(t, b)
}

func TestNewBackend_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	newDir := filepath.Join(dir, "sub", "deep")
	b, err := NewBackend(newDir)
	require.NoError(t, err)
	assert.NotNil(t, b)
}

// ---------------------------------------------------------------------------
// Put / Get round-trip
// ---------------------------------------------------------------------------

func TestBackend_PutGet(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	err := b.Put(ctx, "test/key", []byte("hello"))
	require.NoError(t, err)

	data, err := b.Get(ctx, "test/key")
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), data)
}

func TestBackend_Put_OverwritesExistingFile(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	require.NoError(t, b.Put(ctx, "file.txt", []byte("original")))
	require.NoError(t, b.Put(ctx, "file.txt", []byte("updated")))

	data, err := b.Get(ctx, "file.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte("updated"), data)
}

func TestBackend_Put_CreatesIntermediateDirectories(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	err := b.Put(ctx, "a/b/c/state.tfstate", []byte(`{"version":4}`))
	require.NoError(t, err)

	data, err := b.Get(ctx, "a/b/c/state.tfstate")
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"version":4}`), data)
}

func TestBackend_Get_NonExistent(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	data, err := b.Get(ctx, "does/not/exist")
	assert.Error(t, err)
	assert.Nil(t, data)
}

func TestBackend_Put_EmptyData(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	err := b.Put(ctx, "empty.txt", []byte{})
	require.NoError(t, err)

	data, err := b.Get(ctx, "empty.txt")
	require.NoError(t, err)
	assert.Equal(t, []byte{}, data)
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestBackend_Delete_ExistingFile(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	require.NoError(t, b.Put(ctx, "del.txt", []byte("bye")))
	require.NoError(t, b.Delete(ctx, "del.txt"))

	_, err := b.Get(ctx, "del.txt")
	assert.Error(t, err)
}

func TestBackend_Delete_NonExistentFile_NoError(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	// Deleting a file that does not exist should be a no-op, not an error
	err := b.Delete(ctx, "ghost.txt")
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Exists
// ---------------------------------------------------------------------------

func TestBackend_Exists_FilePresent(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	require.NoError(t, b.Put(ctx, "present.txt", []byte("data")))

	exists, err := b.Exists(ctx, "present.txt")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestBackend_Exists_FileAbsent(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	exists, err := b.Exists(ctx, "absent.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestBackend_Exists_AfterDelete(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	require.NoError(t, b.Put(ctx, "temp.txt", []byte("x")))
	require.NoError(t, b.Delete(ctx, "temp.txt"))

	exists, err := b.Exists(ctx, "temp.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestBackend_List_DirectoryPrefix(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	require.NoError(t, b.Put(ctx, "org/ws1/state.tfstate", []byte("a")))
	require.NoError(t, b.Put(ctx, "org/ws2/state.tfstate", []byte("b")))
	require.NoError(t, b.Put(ctx, "other/ws3/state.tfstate", []byte("c")))

	results, err := b.List(ctx, "org")
	require.NoError(t, err)

	sort.Strings(results)
	assert.Equal(t, []string{
		filepath.Join("org", "ws1", "state.tfstate"),
		filepath.Join("org", "ws2", "state.tfstate"),
	}, results)
}

func TestBackend_List_EmptyDirectory(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	results, err := b.List(ctx, "nothing")
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestBackend_List_FileNamePrefix(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	require.NoError(t, b.Put(ctx, "keys/abc-1.json", []byte("1")))
	require.NoError(t, b.Put(ctx, "keys/abc-2.json", []byte("2")))
	require.NoError(t, b.Put(ctx, "keys/xyz-1.json", []byte("3")))

	// "keys/abc" is treated as a filename prefix in the "keys/" directory
	results, err := b.List(ctx, "keys/abc")
	require.NoError(t, err)

	sort.Strings(results)
	assert.Equal(t, []string{
		filepath.Join("keys", "abc-1.json"),
		filepath.Join("keys", "abc-2.json"),
	}, results)
}

// ---------------------------------------------------------------------------
// Reader
// ---------------------------------------------------------------------------

func TestBackend_Reader_ValidFile(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	payload := []byte("streaming content")
	require.NoError(t, b.Put(ctx, "stream.txt", payload))

	rc, err := b.Reader(ctx, "stream.txt")
	require.NoError(t, err)
	defer rc.Close()

	buf := make([]byte, len(payload))
	n, err := rc.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, len(payload), n)
	assert.Equal(t, payload, buf)
}

func TestBackend_Reader_NonExistent(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	rc, err := b.Reader(ctx, "no-such-file.txt")
	assert.Error(t, err)
	assert.Nil(t, rc)
}

// ---------------------------------------------------------------------------
// Path traversal prevention
// ---------------------------------------------------------------------------

func TestBackend_PathTraversal_IsRejected(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	// Attempt to escape the base directory via ".."
	_, err := b.Get(ctx, "../../etc/passwd")
	assert.Error(t, err, "path traversal should be rejected by Get")

	err = b.Put(ctx, "../../tmp/evil.txt", []byte("evil"))
	assert.Error(t, err, "path traversal should be rejected by Put")

	err = b.Delete(ctx, "../../tmp/evil.txt")
	assert.Error(t, err, "path traversal should be rejected by Delete")

	_, err = b.Exists(ctx, "../../tmp/evil.txt")
	assert.Error(t, err, "path traversal should be rejected by Exists")
}
