package statesource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// s3conn reads state from an S3 bucket (or S3-compatible store such as MinIO /
// DigitalOcean Spaces via config.endpoint). Static credentials are used when
// supplied; otherwise the default AWS credential chain applies.
type s3conn struct {
	client *s3.Client
	bucket string
	prefix string
}

func newS3(config, creds map[string]any) (*s3conn, error) {
	bucket, _ := config["bucket"].(string)
	if bucket == "" {
		return nil, fmt.Errorf("s3 source requires config.bucket")
	}
	region, _ := config["region"].(string)
	if region == "" {
		region = "us-east-1"
	}
	prefix, _ := config["prefix"].(string)
	endpoint, _ := config["endpoint"].(string)
	accessKey, _ := creds["access_key_id"].(string)
	secretKey, _ := creds["secret_access_key"].(string)

	// A half-specified static credential (one of the two blank — a typo, or a
	// secret that resolved to empty) must be an error, not a silent fall-back to
	// the ambient AWS chain (the pod/instance role), which could operate against
	// an unintended account: a confused-deputy read/write. Require both or neither.
	if (accessKey == "") != (secretKey == "") {
		return nil, fmt.Errorf("s3 source requires both access_key_id and secret_access_key (or neither, to use the ambient AWS credential chain)")
	}

	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if accessKey != "" && secretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to configure S3 client: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
	})
	return &s3conn{client: client, bucket: bucket, prefix: prefix}, nil
}

func (s *s3conn) List(ctx context.Context) ([]StateRef, error) {
	in := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket)}
	if s.prefix != "" {
		in.Prefix = aws.String(s.prefix)
	}
	var refs []StateRef
	p := s3.NewListObjectsV2Paginator(s.client, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			key := aws.ToString(o.Key)
			if !strings.HasSuffix(key, ".tfstate") {
				continue
			}
			ref := StateRef{Key: key, Name: key, Size: aws.ToInt64(o.Size), LastModified: o.LastModified}
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

func (s *s3conn) Read(ctx context.Context, key string) (*RawState, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return nil, fmt.Errorf("state s3://%s/%s %w", s.bucket, key, ErrNotFound)
		}
		return nil, fmt.Errorf("failed to read s3://%s/%s: %w", s.bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := readCapped(out.Body)
	if err != nil {
		return nil, err
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data)), LastModified: out.LastModified}, nil
}

func (s *s3conn) Write(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("failed to write s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// Delete removes the object at key. S3's DeleteObject is idempotent (a missing
// key still succeeds); the edit pipeline's pre-delete read enforces the
// not-found case before this is called.
func (s *s3conn) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("failed to delete s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}
