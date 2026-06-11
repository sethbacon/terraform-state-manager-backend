package statesource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalListAndRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prod.tfstate"), []byte(`{"version":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "team")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "dev.tfstate"), []byte(`{"version":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(`ignore me`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := New("local", map[string]any{"base_path": dir}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	refs, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 .tfstate files, got %d (%+v)", len(refs), refs)
	}

	rs, err := c.Read(context.Background(), "team/dev.tfstate")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(rs.Data) != `{"version":4}` {
		t.Errorf("unexpected data: %s", rs.Data)
	}
}

func TestLocalRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	c, err := New("local", map[string]any{"base_path": dir}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Read(context.Background(), "../../etc/passwd"); err == nil {
		t.Error("expected traversal to be rejected")
	}
}

func TestLocalRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.tfstate")
	if err := os.WriteFile(secret, []byte(`{"version":4}`), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	// A symlink INSIDE the base directory that points outside it must not allow
	// reads/writes to escape the base (lexical ../ checks alone miss this).
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	c, err := New("local", map[string]any{"base_path": base}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Read(context.Background(), "escape/secret.tfstate"); err == nil {
		t.Error("expected symlink escape to be rejected on read")
	}
	if err := c.Write(context.Background(), "escape/pwned.tfstate", []byte(`{}`)); err == nil {
		t.Error("expected symlink escape to be rejected on write")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.tfstate")); err == nil {
		t.Error("write escaped the base directory")
	}
}

func TestLocalWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prod.tfstate"), []byte(`{"version":4,"serial":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New("local", map[string]any{"base_path": dir}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	updated := []byte(`{"version":4,"serial":2}`)
	if err := c.Write(context.Background(), "prod.tfstate", updated); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rs, err := c.Read(context.Background(), "prod.tfstate")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(rs.Data) != string(updated) {
		t.Fatalf("round trip mismatch: got %s", rs.Data)
	}
	// Traversal keys are rejected outright (previously they were silently
	// confined to basePath, relocating the write to a surprising path).
	if err := c.Write(context.Background(), "../escape.tfstate", []byte("x")); err == nil {
		t.Fatal("traversal write must be rejected")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.tfstate")); !os.IsNotExist(err) {
		t.Error("traversal write escaped basePath")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.tfstate")); !os.IsNotExist(err) {
		t.Error("rejected key must write nothing at all")
	}
}

func TestNewUnsupportedType(t *testing.T) {
	if _, err := New("hcp", nil, nil); err == nil {
		t.Error("expected hcp to require config/credentials")
	}
	if _, err := New("bogus", nil, nil); err == nil {
		t.Error("expected unknown type error")
	}
}

func TestLocalWriteCreatesNestedDirectories(t *testing.T) {
	dir := t.TempDir()
	l, err := newLocal(map[string]any{"base_path": dir})
	if err != nil {
		t.Fatalf("newLocal: %v", err)
	}

	// Transfer targets may name not-yet-existing subdirectories.
	if err := l.Write(context.Background(), "envs/prod/site.tfstate", []byte(`{"version":4}`)); err != nil {
		t.Fatalf("nested write: %v", err)
	}
	rs, err := l.Read(context.Background(), "envs/prod/site.tfstate")
	if err != nil || string(rs.Data) != `{"version":4}` {
		t.Fatalf("read-back: %v (%s)", err, rs.Data)
	}

	// Traversal stays rejected even with directory creation in play.
	if err := l.Write(context.Background(), "../outside.tfstate", []byte("{}")); err == nil {
		t.Error("traversal key must be rejected")
	}
}
