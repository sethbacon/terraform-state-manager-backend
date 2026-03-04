// Package s3 implements a Terraform state file scanner for S3-compatible
// object storage (AWS S3, MinIO, DigitalOcean Spaces, etc.).
//
// To avoid pulling in the full AWS SDK, this client talks directly to the S3
// REST API using AWS Signature Version 4 signing over net/http.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/azure"
)

// Config holds the parameters needed to connect to an S3-compatible bucket.
type Config struct {
	// Bucket is the name of the S3 bucket containing state files.
	Bucket string `json:"bucket"`
	// Region is the AWS region (e.g. "us-east-1").
	Region string `json:"region"`
	// Prefix filters objects whose keys start with this value.
	Prefix string `json:"prefix,omitempty"`
	// AccessKeyID is the AWS access key.
	AccessKeyID string `json:"access_key_id,omitempty"`
	// SecretAccessKey is the AWS secret key.
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	// Endpoint overrides the default AWS S3 endpoint (useful for MinIO, etc.).
	Endpoint string `json:"endpoint,omitempty"`
}

// Client communicates with S3-compatible storage through the REST API.
type Client struct {
	config     Config
	httpClient *http.Client
}

// NewClient validates the supplied configuration and returns a ready-to-use
// Client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("s3: region is required")
	}

	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// bucketURL returns the endpoint for the configured bucket, using either the
// custom endpoint or the standard AWS virtual-hosted-style URL.
func (c *Client) bucketURL() string {
	if c.config.Endpoint != "" {
		ep := strings.TrimRight(c.config.Endpoint, "/")
		return fmt.Sprintf("%s/%s", ep, c.config.Bucket)
	}
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com", c.config.Bucket, c.config.Region)
}

// objectURL returns the full URL for a specific object key.
func (c *Client) objectURL(key string) string {
	return fmt.Sprintf("%s/%s", c.bucketURL(), key)
}

// TestConnection verifies bucket access by issuing a HEAD request against the
// bucket URL.
func (c *Client) TestConnection(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.bucketURL()+"/", nil)
	if err != nil {
		return fmt.Errorf("s3: failed to create request: %w", err)
	}

	c.signRequest(req, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3: connection test failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("s3: connection test returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// listBucketResult mirrors the XML response from the S3 ListObjectsV2 API.
type listBucketResult struct {
	XMLName               xml.Name   `xml:"ListBucketResult"`
	Contents              []s3Object `xml:"Contents"`
	IsTruncated           bool       `xml:"IsTruncated"`
	NextContinuationToken string     `xml:"NextContinuationToken"`
}

type s3Object struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	Size         int64  `xml:"Size"`
}

// ListStateFiles enumerates all objects in the bucket whose keys start with the
// configured prefix and end with ".tfstate".  Pagination is handled
// transparently using the ListObjectsV2 continuation token.
func (c *Client) ListStateFiles(ctx context.Context) ([]azure.StateFileRef, error) {
	var refs []azure.StateFileRef
	continuationToken := ""

	for {
		params := url.Values{
			"list-type": {"2"},
		}
		if c.config.Prefix != "" {
			params.Set("prefix", c.config.Prefix)
		}
		if continuationToken != "" {
			params.Set("continuation-token", continuationToken)
		}

		reqURL := fmt.Sprintf("%s/?%s", c.bucketURL(), params.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("s3: failed to create list request: %w", err)
		}

		c.signRequest(req, nil)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("s3: list objects request failed: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3: failed to read list response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("s3: list objects returned status %d: %s", resp.StatusCode, string(body))
		}

		var result listBucketResult
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("s3: failed to parse list response XML: %w", err)
		}

		for _, obj := range result.Contents {
			if !strings.HasSuffix(obj.Key, ".tfstate") {
				continue
			}
			lastMod, _ := time.Parse(time.RFC3339, obj.LastModified)
			refs = append(refs, azure.StateFileRef{
				Key:          obj.Key,
				LastModified: lastMod,
				Size:         obj.Size,
			})
		}

		if !result.IsTruncated {
			break
		}
		continuationToken = result.NextContinuationToken
	}

	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Key < refs[j].Key
	})

	return refs, nil
}

// DownloadState retrieves the raw content of the object referenced by ref.
func (c *Client) DownloadState(ctx context.Context, ref azure.StateFileRef) ([]byte, error) {
	reqURL := c.objectURL(ref.Key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("s3: failed to create download request: %w", err)
	}

	c.signRequest(req, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3: download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("s3: download returned status %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("s3: failed to read object content: %w", err)
	}

	return data, nil
}

// ---------------------------------------------------------------------------
// AWS Signature Version 4 signing
// ---------------------------------------------------------------------------

// signRequest adds AWS SigV4 Authorization headers to the request.  The
// payload parameter is the raw request body (nil for GET/HEAD).
func (c *Client) signRequest(req *http.Request, payload []byte) {
	now := time.Now().UTC()
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	service := "s3"

	// Hash the payload (empty string hash for GET/HEAD).
	payloadHash := sha256Hex(payload)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("Host", req.URL.Host)

	// 1. Create canonical request.
	signedHeaders, canonicalHeaders := c.buildCanonicalHeaders(req)
	canonicalURI := req.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQueryString := c.buildCanonicalQueryString(req.URL.Query())

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	// 2. Create string to sign.
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, c.config.Region, service)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	// 3. Calculate signature.
	signingKey := c.deriveSigningKey(datestamp, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// 4. Build Authorization header.
	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.config.AccessKeyID, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", auth)
}

// buildCanonicalHeaders returns the signed-headers list and the canonical
// header block for the SigV4 canonical request.
func (c *Client) buildCanonicalHeaders(req *http.Request) (string, string) {
	headers := map[string]string{
		"host": req.URL.Host,
	}
	for k, v := range req.Header {
		lower := strings.ToLower(k)
		if lower == "x-amz-content-sha256" || lower == "x-amz-date" {
			headers[lower] = strings.TrimSpace(v[0])
		}
	}

	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canonical strings.Builder
	for _, k := range keys {
		canonical.WriteString(k)
		canonical.WriteString(":")
		canonical.WriteString(headers[k])
		canonical.WriteString("\n")
	}

	return strings.Join(keys, ";"), canonical.String()
}

// buildCanonicalQueryString URI-encodes and lexicographically sorts query
// parameters.
func (c *Client) buildCanonicalQueryString(params url.Values) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		for _, v := range params[k] {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// deriveSigningKey produces the SigV4 signing key using a chain of HMAC-SHA256
// operations.
func (c *Client) deriveSigningKey(datestamp, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+c.config.SecretAccessKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(c.config.Region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// sha256Hex returns the hex-encoded SHA-256 hash of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// hmacSHA256 computes the HMAC-SHA256 of msg using the given key.
func hmacSHA256(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}
