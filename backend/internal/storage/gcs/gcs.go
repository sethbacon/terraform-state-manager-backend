// Package gcs implements the storage.Backend interface using Google Cloud Storage.
package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	gcsstorage "cloud.google.com/go/storage"
	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// Backend stores and retrieves objects in a Google Cloud Storage bucket.
type Backend struct {
	bucket *gcsstorage.BucketHandle
	prefix string
}

// New creates a new GCS storage backend using the provided configuration.
//
// Credential resolution order:
//  1. CredentialsJSON starts with '{' — treated as raw service-account JSON.
//  2. CredentialsJSON is non-empty but is not JSON — treated as a file path.
//  3. CredentialsFile is non-empty — used as a credentials file path.
//  4. Otherwise Application Default Credentials (ADC) are used.
func New(ctx context.Context, cfg config.GCSStorageConfig) (*Backend, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("gcs: bucket name must not be empty")
	}

	var opts []option.ClientOption

	switch {
	case cfg.CredentialsJSON != "" && strings.HasPrefix(cfg.CredentialsJSON, "{"):
		opts = append(opts, option.WithCredentialsJSON([]byte(cfg.CredentialsJSON)))
	case cfg.CredentialsJSON != "":
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsJSON))
	case cfg.CredentialsFile != "":
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsFile))
	// default: ADC — no option needed
	}

	if cfg.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(cfg.Endpoint))
	}

	client, err := gcsstorage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gcs: failed to create GCS client: %w", err)
	}

	return &Backend{
		bucket: client.Bucket(cfg.Bucket),
		prefix: "",
	}, nil
}

// objectKey builds the full GCS object key by joining the bucket prefix (if
// any) with the caller-supplied key, stripping any leading slash.
func (b *Backend) objectKey(key string) string {
	if b.prefix == "" {
		return key
	}
	return strings.TrimPrefix(b.prefix+"/"+key, "/")
}

// Put writes data to the given key in the GCS bucket.
func (b *Backend) Put(ctx context.Context, key string, data []byte) error {
	wc := b.bucket.Object(b.objectKey(key)).NewWriter(ctx)

	if _, err := io.Copy(wc, bytes.NewReader(data)); err != nil {
		// Attempt to close (and discard) the writer before returning.
		_ = wc.Close()
		return fmt.Errorf("gcs: failed to write object %q: %w", key, err)
	}

	// Close commits the object; errors here indicate a write failure.
	if err := wc.Close(); err != nil {
		return fmt.Errorf("gcs: failed to close writer for object %q: %w", key, err)
	}

	return nil
}

// Get retrieves the full contents of the object at key.
func (b *Backend) Get(ctx context.Context, key string) ([]byte, error) {
	rc, err := b.bucket.Object(b.objectKey(key)).NewReader(ctx)
	if err != nil {
		if errors.Is(err, gcsstorage.ErrObjectNotExist) {
			return nil, fmt.Errorf("gcs: object not found: %s", key)
		}
		return nil, fmt.Errorf("gcs: failed to open reader for object %q: %w", key, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("gcs: failed to read object %q: %w", key, err)
	}

	return data, nil
}

// Delete removes the object at key from the GCS bucket. If the object does not
// exist the call is treated as a no-op.
func (b *Backend) Delete(ctx context.Context, key string) error {
	err := b.bucket.Object(b.objectKey(key)).Delete(ctx)
	if err != nil {
		if errors.Is(err, gcsstorage.ErrObjectNotExist) {
			return nil // already gone
		}
		return fmt.Errorf("gcs: failed to delete object %q: %w", key, err)
	}
	return nil
}

// List returns the object keys inside the bucket whose names share the given
// prefix. The returned paths are bare GCS object names (not full URIs).
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	fullPrefix := b.objectKey(prefix)

	query := &gcsstorage.Query{Prefix: fullPrefix}
	it := b.bucket.Objects(ctx, query)

	var keys []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: error iterating objects with prefix %q: %w", prefix, err)
		}
		keys = append(keys, attrs.Name)
	}

	return keys, nil
}

// Exists reports whether an object exists at the given key.
func (b *Backend) Exists(ctx context.Context, key string) (bool, error) {
	_, err := b.bucket.Object(b.objectKey(key)).Attrs(ctx)
	if err != nil {
		if errors.Is(err, gcsstorage.ErrObjectNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("gcs: failed to stat object %q: %w", key, err)
	}
	return true, nil
}

// Reader returns an io.ReadCloser for streaming reads of the object at key.
func (b *Backend) Reader(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := b.bucket.Object(b.objectKey(key)).NewReader(ctx)
	if err != nil {
		if errors.Is(err, gcsstorage.ErrObjectNotExist) {
			return nil, fmt.Errorf("gcs: object not found: %s", key)
		}
		return nil, fmt.Errorf("gcs: failed to open reader for object %q: %w", key, err)
	}
	return rc, nil
}
