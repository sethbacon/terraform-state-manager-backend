package statesource

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// local reads .tfstate files from a base directory on the server. The base path
// is operator-configured (creating a source requires the sources:manage scope);
// keys are validated to stay within the base directory.
type local struct {
	basePath string
}

func newLocal(config map[string]any) (*local, error) {
	bp, _ := config["base_path"].(string)
	if bp == "" {
		return nil, fmt.Errorf("local source requires config.base_path")
	}
	abs, err := filepath.Abs(bp)
	if err != nil {
		return nil, fmt.Errorf("invalid base_path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("base_path %q is not accessible: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("base_path %q is not a directory", abs)
	}
	// Canonicalize so symlink containment checks in resolve() compare real paths.
	if real, evErr := filepath.EvalSymlinks(abs); evErr == nil {
		abs = real
	}
	return &local{basePath: abs}, nil
}

func (l *local) List(_ context.Context) ([]StateRef, error) {
	var refs []StateRef
	err := filepath.WalkDir(l.basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".tfstate") {
			return nil
		}
		rel, relErr := filepath.Rel(l.basePath, path)
		if relErr != nil {
			return nil
		}
		ref := StateRef{Key: filepath.ToSlash(rel), Name: filepath.ToSlash(rel)}
		if info, infoErr := d.Info(); infoErr == nil {
			ref.Size = info.Size()
			mod := info.ModTime()
			ref.LastModified = &mod
		}
		refs = append(refs, ref)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
}

func (l *local) Read(_ context.Context, key string) (*RawState, error) {
	full, err := l.resolve(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full) // #nosec G304 -- path is validated to stay within basePath by resolve()
	if err != nil {
		return nil, fmt.Errorf("failed to read state %q: %w", key, err)
	}
	rs := &RawState{Key: key, Data: data, Size: int64(len(data))}
	if info, statErr := os.Stat(full); statErr == nil {
		mod := info.ModTime()
		rs.LastModified = &mod
	}
	return rs, nil
}

// Write atomically replaces the state file at key (temp file + rename).
func (l *local) Write(_ context.Context, key string, data []byte) error {
	full, err := l.resolve(key)
	if err != nil {
		return err
	}
	// New keys may live in not-yet-existing subdirectories (transfer targets
	// like envs/prod/app.tfstate); resolve() already confined the path to the
	// base directory.
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return fmt.Errorf("failed to create directories for %q: %w", key, err)
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // #nosec G304 -- path validated by resolve()
		return fmt.Errorf("failed to write state %q: %w", key, err)
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to commit state %q: %w", key, err)
	}
	return nil
}

// Delete removes the state file at key. A missing file is reported as ErrNotFound
// so callers can distinguish it from a backend failure.
func (l *local) Delete(_ context.Context, key string) error {
	full, err := l.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("state %q %w", key, ErrNotFound)
		}
		return fmt.Errorf("failed to delete state %q: %w", key, err)
	}
	return nil
}

// Lock creates an exclusive lock file next to the state; it fails if one already
// exists. Returns a lock id that must be presented to Unlock.
func (l *local) Lock(_ context.Context, key string) (string, error) {
	lp, err := l.lockPath(key)
	if err != nil {
		return "", err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	lockID := hex.EncodeToString(id)
	f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- path validated by resolve()
	if err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("state %q is locked", key)
		}
		return "", err
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(lockID)
	return lockID, nil
}

// Unlock removes the lock file when the lock id matches.
func (l *local) Unlock(_ context.Context, key, lockID string) error {
	lp, err := l.lockPath(key)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(lp) // #nosec G304 -- path validated by resolve()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(string(data)) != lockID {
		return fmt.Errorf("lock id mismatch for %q", key)
	}
	return os.Remove(lp)
}

func (l *local) lockPath(key string) (string, error) {
	full, err := l.resolve(key)
	if err != nil {
		return "", err
	}
	return full + ".tsmlock", nil
}

// resolve maps a key to an absolute path, rejecting traversal outside basePath
// via both lexical "../" stripping and symlink resolution (a symlink inside the
// base directory must not redirect reads/writes outside it).
func (l *local) resolve(key string) (string, error) {
	// Reject traversal segments outright rather than silently relocating the
	// key inside the base after the force-root clean.
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return "", fmt.Errorf("invalid state key %q (path traversal)", key)
		}
	}
	clean := filepath.Clean("/" + filepath.FromSlash(key)) // force-root then clean
	full := filepath.Join(l.basePath, clean)
	if full != l.basePath && !strings.HasPrefix(full, l.basePath+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid state key %q", key)
	}
	if err := l.ensureWithinBase(full); err != nil {
		return "", err
	}
	return full, nil
}

// ensureWithinBase resolves symlinks on the deepest existing ancestor of full
// (the target file itself may not exist yet for writes) and requires the real
// path to remain inside basePath, defeating symlink escapes.
func (l *local) ensureWithinBase(full string) error {
	dir := full
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("invalid state key path: %w", err)
	}
	if real != l.basePath && !strings.HasPrefix(real, l.basePath+string(os.PathSeparator)) {
		return fmt.Errorf("state key path escapes base directory")
	}
	return nil
}
