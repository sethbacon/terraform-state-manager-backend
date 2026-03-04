// Package local implements the storage.Backend interface using the local filesystem.
package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Backend stores and retrieves files on the local filesystem under a
// configurable base directory.
type Backend struct {
	basePath string
}

// NewBackend creates a new local storage backend. It ensures the base directory
// exists (creating it if necessary).
func NewBackend(basePath string) (*Backend, error) {
	if basePath == "" {
		return nil, fmt.Errorf("local storage base path must not be empty")
	}

	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute path for %q: %w", basePath, err)
	}

	if err := os.MkdirAll(absPath, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create base directory %q: %w", absPath, err)
	}

	return &Backend{basePath: absPath}, nil
}

// fullPath resolves a storage path relative to the base directory and ensures
// the result stays within the base directory (prevents path traversal).
func (b *Backend) fullPath(storagePath string) (string, error) {
	full := filepath.Join(b.basePath, storagePath)
	full = filepath.Clean(full)

	// Ensure the resolved path is still under basePath.
	rel, err := filepath.Rel(b.basePath, full)
	if err != nil || len(rel) >= 2 && rel[:2] == ".." {
		return "", fmt.Errorf("path %q escapes base directory", storagePath)
	}
	return full, nil
}

// Put writes data to the given path under the base directory.
func (b *Backend) Put(_ context.Context, path string, data []byte) error {
	full, err := b.fullPath(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("failed to create directory %q: %w", dir, err)
	}

	if err := os.WriteFile(full, data, 0o640); err != nil {
		return fmt.Errorf("failed to write file %q: %w", full, err)
	}
	return nil
}

// Get reads and returns the full contents of the file at the given path.
func (b *Backend) Get(_ context.Context, path string) ([]byte, error) {
	full, err := b.fullPath(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("file not found: %s", path)
		}
		return nil, fmt.Errorf("failed to read file %q: %w", full, err)
	}
	return data, nil
}

// Delete removes the file at the given path and cleans up any empty parent
// directories up to (but not including) the base directory.
func (b *Backend) Delete(_ context.Context, path string) error {
	full, err := b.fullPath(path)
	if err != nil {
		return err
	}

	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return nil // already gone
		}
		return fmt.Errorf("failed to delete file %q: %w", full, err)
	}

	// Remove empty parent directories up to basePath.
	dir := filepath.Dir(full)
	for dir != b.basePath {
		if err := os.Remove(dir); err != nil {
			break // not empty or permission error — stop climbing
		}
		dir = filepath.Dir(dir)
	}

	return nil
}

// List returns file paths under the base directory that match the given prefix.
func (b *Backend) List(_ context.Context, prefix string) ([]string, error) {
	searchDir := filepath.Join(b.basePath, prefix)
	searchDir = filepath.Clean(searchDir)

	// If prefix points to a directory, walk it; otherwise treat it as a
	// file-name prefix in its parent directory.
	info, err := os.Stat(searchDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to stat prefix path %q: %w", searchDir, err)
	}

	var results []string

	if err == nil && info.IsDir() {
		// Walk the directory.
		walkErr := filepath.Walk(searchDir, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil // skip errors
			}
			if fi.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(b.basePath, path)
			if relErr != nil {
				return nil
			}
			results = append(results, rel)
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("failed to walk directory %q: %w", searchDir, walkErr)
		}
	} else {
		// Treat prefix as a file-name prefix in its parent directory.
		dir := filepath.Dir(searchDir)
		base := filepath.Base(searchDir)
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return results, nil
			}
			return nil, fmt.Errorf("failed to read directory %q: %w", dir, readErr)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if strings.HasPrefix(entry.Name(), base) {
				rel, relErr := filepath.Rel(b.basePath, filepath.Join(dir, entry.Name()))
				if relErr != nil {
					continue
				}
				results = append(results, rel)
			}
		}
	}

	return results, nil
}

// Exists reports whether a file exists at the given path.
func (b *Backend) Exists(_ context.Context, path string) (bool, error) {
	full, err := b.fullPath(path)
	if err != nil {
		return false, err
	}

	_, err = os.Stat(full)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to stat file %q: %w", full, err)
}

// Reader returns an io.ReadCloser for streaming reads of the file at path.
func (b *Backend) Reader(_ context.Context, path string) (io.ReadCloser, error) {
	full, err := b.fullPath(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("file not found: %s", path)
		}
		return nil, fmt.Errorf("failed to open file %q: %w", full, err)
	}
	return f, nil
}
