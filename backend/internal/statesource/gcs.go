package statesource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// gcsAPI abstracts the GCS SDK operations the connector uses so tests can
// inject a mock (registry storage-backend pattern); realGCSClient adapts the
// actual *storage.Client behind it.
type gcsAPI interface {
	Objects(ctx context.Context, bucket string, q *storage.Query) gcsObjectIterator
	NewReader(ctx context.Context, bucket, object string) (io.ReadCloser, error)
	// NewWriter returns a writer for the object with the given content type
	// already applied; the upload commits on Close.
	NewWriter(ctx context.Context, bucket, object, contentType string) io.WriteCloser
}

// gcsObjectIterator is the iteration subset of *storage.ObjectIterator.
type gcsObjectIterator interface {
	Next() (*storage.ObjectAttrs, error)
}

// realGCSClient delegates gcsAPI to the GCS SDK.
type realGCSClient struct {
	client *storage.Client
}

// coverage:skip:trivial-delegation
func (r *realGCSClient) Objects(ctx context.Context, bucket string, q *storage.Query) gcsObjectIterator {
	return r.client.Bucket(bucket).Objects(ctx, q)
}

// coverage:skip:trivial-delegation
func (r *realGCSClient) NewReader(ctx context.Context, bucket, object string) (io.ReadCloser, error) {
	return r.client.Bucket(bucket).Object(object).NewReader(ctx)
}

// coverage:skip:trivial-delegation
func (r *realGCSClient) NewWriter(ctx context.Context, bucket, object, contentType string) io.WriteCloser {
	w := r.client.Bucket(bucket).Object(object).NewWriter(ctx)
	w.ContentType = contentType
	return w
}

// gcsConn reads/writes state in a Google Cloud Storage bucket. A service-account
// JSON key may be supplied via credentials.credentials_json; otherwise Application
// Default Credentials are used.
type gcsConn struct {
	client gcsAPI
	bucket string
	prefix string
}

func newGCS(config, creds map[string]any) (*gcsConn, error) {
	bucket, _ := config["bucket"].(string)
	if bucket == "" {
		return nil, fmt.Errorf("gcs source requires config.bucket")
	}
	prefix, _ := config["prefix"].(string)

	var opts []option.ClientOption
	if jsonKey, _ := creds["credentials_json"].(string); jsonKey != "" {
		opts = append(opts, option.WithCredentialsJSON([]byte(jsonKey)))
	}
	client, err := storage.NewClient(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}
	return &gcsConn{client: &realGCSClient{client: client}, bucket: bucket, prefix: prefix}, nil
}

func (g *gcsConn) List(ctx context.Context) ([]StateRef, error) {
	it := g.client.Objects(ctx, g.bucket, &storage.Query{Prefix: g.prefix})
	var refs []StateRef
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(attrs.Name, ".tfstate") {
			continue
		}
		ref := StateRef{Key: attrs.Name, Name: attrs.Name, Size: attrs.Size}
		mod := attrs.Updated
		ref.LastModified = &mod
		refs = append(refs, ref)
	}
	return refs, nil
}

func (g *gcsConn) Read(ctx context.Context, key string) (*RawState, error) {
	r, err := g.client.NewReader(ctx, g.bucket, key)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, fmt.Errorf("state gs://%s/%s %w", g.bucket, key, ErrNotFound)
		}
		return nil, fmt.Errorf("failed to read gs://%s/%s: %w", g.bucket, key, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data))}, nil
}

func (g *gcsConn) Write(ctx context.Context, key string, data []byte) error {
	w := g.client.NewWriter(ctx, g.bucket, key, "application/json")
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return fmt.Errorf("failed to write gs://%s/%s: %w", g.bucket, key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to finalize gs://%s/%s: %w", g.bucket, key, err)
	}
	return nil
}
