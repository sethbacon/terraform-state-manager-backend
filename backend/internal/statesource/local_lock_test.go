package statesource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newLocalAt(t *testing.T) (*local, string) {
	t.Helper()
	dir := t.TempDir()
	conn, err := newLocal(map[string]any{"base_path": dir})
	if err != nil {
		t.Fatalf("newLocal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.tfstate"), []byte(`{"version":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return conn, dir
}

func TestLocal_LockUnlock(t *testing.T) {
	conn, dir := newLocalAt(t)
	ctx := context.Background()

	id, err := conn.Lock(ctx, "app.tfstate")
	if err != nil || id == "" {
		t.Fatalf("Lock: %v %q", err, id)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.tfstate.tsmlock")); err != nil {
		t.Errorf("lock file missing: %v", err)
	}

	// Second writer is excluded while held.
	if _, err := conn.Lock(ctx, "app.tfstate"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("double lock: %v", err)
	}

	// Unlock with the wrong id must not release someone else's lock.
	if err := conn.Unlock(ctx, "app.tfstate", "wrong-id"); err == nil {
		t.Error("unlock with mismatched id must error")
	}
	if err := conn.Unlock(ctx, "app.tfstate", id); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.tfstate.tsmlock")); !os.IsNotExist(err) {
		t.Error("lock file not removed")
	}

	// Unlocking an already-released lock is idempotent.
	if err := conn.Unlock(ctx, "app.tfstate", id); err != nil {
		t.Errorf("idempotent unlock: %v", err)
	}

	// Re-lockable after release.
	if _, err := conn.Lock(ctx, "app.tfstate"); err != nil {
		t.Errorf("relock after unlock: %v", err)
	}
}

func TestLocal_LockContainsTraversal(t *testing.T) {
	// resolve() rejects traversal keys outright (it used to silently confine
	// them to the base, which made the effective path surprising).
	conn, dir := newLocalAt(t)
	if _, err := conn.Lock(context.Background(), "../outside.tfstate"); err == nil {
		t.Fatal("traversal key must be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "outside.tfstate.tsmlock")); !os.IsNotExist(err) {
		t.Error("no lock file may be created for a rejected key")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "outside.tfstate.tsmlock")); !os.IsNotExist(err) {
		t.Error("lock file escaped the base path")
	}
}
