// Package storage defines the Backend interface for object storage operations
// used by the backup and migration subsystems.
package storage

import (
	"context"
	"io"
)

// Backend is the interface that all storage backends (local, S3, Azure, GCS)
// must implement for reading and writing backup and state files.
type Backend interface {
	// Put writes data to the specified path in the storage backend.
	Put(ctx context.Context, path string, data []byte) error

	// Get retrieves data from the specified path in the storage backend.
	Get(ctx context.Context, path string) ([]byte, error)

	// Delete removes the object at the specified path from the storage backend.
	Delete(ctx context.Context, path string) error

	// List returns object paths matching the given prefix.
	List(ctx context.Context, prefix string) ([]string, error)

	// Exists checks whether an object exists at the given path.
	Exists(ctx context.Context, path string) (bool, error)

	// Reader returns an io.ReadCloser for streaming reads of the object at path.
	Reader(ctx context.Context, path string) (io.ReadCloser, error)
}
