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

// gcsConn reads/writes state in a Google Cloud Storage bucket. A service-account
// JSON key may be supplied via credentials.credentials_json; otherwise Application
// Default Credentials are used.
type gcsConn struct {
	client *storage.Client
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
	return &gcsConn{client: client, bucket: bucket, prefix: prefix}, nil
}

func (g *gcsConn) List(ctx context.Context) ([]StateRef, error) {
	it := g.client.Bucket(g.bucket).Objects(ctx, &storage.Query{Prefix: g.prefix})
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
	r, err := g.client.Bucket(g.bucket).Object(key).NewReader(ctx)
	if err != nil {
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
	w := g.client.Bucket(g.bucket).Object(key).NewWriter(ctx)
	w.ContentType = "application/json"
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return fmt.Errorf("failed to write gs://%s/%s: %w", g.bucket, key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to finalize gs://%s/%s: %w", g.bucket, key, err)
	}
	return nil
}
