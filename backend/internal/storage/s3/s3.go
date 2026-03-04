// Package s3storage implements the storage.Backend interface using Amazon S3.
package s3storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Config holds the configuration for the S3 storage backend.
type Config struct {
	Bucket          string
	Region          string
	Prefix          string // Optional key prefix
	AccessKeyID     string
	SecretAccessKey string
	Endpoint        string // Optional endpoint override (for S3-compatible stores)
	ForcePathStyle  bool
}

// Backend implements storage.Backend using Amazon S3.
type Backend struct {
	client *s3.Client
	bucket string
	prefix string
}

// New creates a new S3 storage backend.
// If AccessKeyID and SecretAccessKey are set, static credentials are used.
// Otherwise the default AWS credential chain is used.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 storage: bucket is required")
	}

	var optFns []func(*config.LoadOptions) error

	if cfg.Region != "" {
		optFns = append(optFns, config.WithRegion(cfg.Region))
	}

	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		optFns = append(optFns, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("s3 storage: failed to load AWS config: %w", err)
	}

	var clientOpts []func(*s3.Options)
	if cfg.Endpoint != "" {
		endpoint := cfg.Endpoint
		pathStyle := cfg.ForcePathStyle
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = pathStyle
		})
	}

	client := s3.NewFromConfig(awsCfg, clientOpts...)

	return &Backend{
		client: client,
		bucket: cfg.Bucket,
		prefix: cfg.Prefix,
	}, nil
}

// objectKey builds the full S3 key incorporating the optional prefix.
func (b *Backend) objectKey(path string) string {
	if b.prefix == "" {
		return path
	}
	return strings.TrimPrefix(b.prefix+"/"+path, "/")
}

// isNotFound returns true if err represents a "key not found" S3 error.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound" || code == "404"
	}
	return false
}

// Put writes data to the given path in the S3 bucket.
func (b *Backend) Put(ctx context.Context, path string, data []byte) error {
	key := b.objectKey(path)
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("s3 storage: failed to put object %q: %w", key, err)
	}
	return nil
}

// Get retrieves the full contents of the object at the given path.
func (b *Backend) Get(ctx context.Context, path string) ([]byte, error) {
	key := b.objectKey(path)
	resp, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("s3 storage: object not found: %s", path)
		}
		return nil, fmt.Errorf("s3 storage: failed to get object %q: %w", key, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("s3 storage: failed to read object %q: %w", key, err)
	}
	return data, nil
}

// Delete removes the object at the given path. If the object does not exist
// the call is treated as a no-op.
func (b *Backend) Delete(ctx context.Context, path string) error {
	key := b.objectKey(path)
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("s3 storage: failed to delete object %q: %w", key, err)
	}
	return nil
}

// List returns object keys whose names have the given prefix (combined with the
// backend's configured prefix).
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	fullPrefix := b.objectKey(prefix)

	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(fullPrefix),
	})

	var results []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 storage: failed to list objects with prefix %q: %w", fullPrefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				results = append(results, *obj.Key)
			}
		}
	}
	return results, nil
}

// Exists reports whether an object exists at the given path.
func (b *Backend) Exists(ctx context.Context, path string) (bool, error) {
	key := b.objectKey(path)
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3 storage: failed to check existence of object %q: %w", key, err)
	}
	return true, nil
}

// Reader returns an io.ReadCloser for streaming reads of the object at the given path.
func (b *Backend) Reader(ctx context.Context, path string) (io.ReadCloser, error) {
	key := b.objectKey(path)
	resp, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("s3 storage: object not found: %s", path)
		}
		return nil, fmt.Errorf("s3 storage: failed to open object %q: %w", key, err)
	}
	return resp.Body, nil
}
