// Package azure implements a Terraform state file scanner for Azure Blob Storage.
//
// Instead of pulling in the full Azure SDK, this client uses the Azure Blob
// Storage REST API directly via net/http.  It supports shared-key authentication
// and can list, download, and probe state files stored in a given container.
package azure

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// StateFileRef is a lightweight reference to a Terraform state file stored in a
// remote backend.  Every scanner client (Azure, S3, GCS, Consul, etc.) returns
// slices of this type so the caller can decide which files to download.
type StateFileRef struct {
	// Key is the path or name of the state file within the backend.
	Key string `json:"key"`
	// LastModified is the timestamp of the last modification.
	LastModified time.Time `json:"last_modified"`
	// Size is the content length in bytes.
	Size int64 `json:"size"`
}

// Config holds the parameters needed to connect to an Azure Blob Storage
// container.
type Config struct {
	// AccountName is the Azure storage account name.
	AccountName string `json:"account_name"`
	// ContainerName is the blob container that holds state files.
	ContainerName string `json:"container_name"`
	// Prefix filters blobs whose names start with this value.
	Prefix string `json:"prefix,omitempty"`
	// Key is the storage account access key used for shared-key auth.
	Key string `json:"key,omitempty"`
}

// Client communicates with Azure Blob Storage through the REST API.
type Client struct {
	config     Config
	httpClient *http.Client
}

// NewClient validates the supplied configuration and returns a ready-to-use
// Client.  It returns an error if required fields are missing.
func NewClient(cfg Config) (*Client, error) {
	if cfg.AccountName == "" {
		return nil, fmt.Errorf("azure: account_name is required")
	}
	if cfg.ContainerName == "" {
		return nil, fmt.Errorf("azure: container_name is required")
	}
	if cfg.Key == "" {
		return nil, fmt.Errorf("azure: storage account key is required")
	}

	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// baseURL returns the Azure Blob Storage endpoint for this account.
func (c *Client) baseURL() string {
	return fmt.Sprintf("https://%s.blob.core.windows.net", c.config.AccountName)
}

// containerURL returns the full URL to the configured container.
func (c *Client) containerURL() string {
	return fmt.Sprintf("%s/%s", c.baseURL(), c.config.ContainerName)
}

// blobURL returns the full URL to a specific blob.
func (c *Client) blobURL(blobName string) string {
	return fmt.Sprintf("%s/%s/%s", c.baseURL(), c.config.ContainerName, blobName)
}

// TestConnection verifies that the configured container is reachable by issuing
// a HEAD request against the container URL.  A non-error return means the
// account and container exist and the credentials are valid.
func (c *Client) TestConnection(ctx context.Context) error {
	reqURL := c.containerURL() + "?restype=container"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, reqURL, nil)
	if err != nil {
		return fmt.Errorf("azure: failed to create request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return fmt.Errorf("azure: failed to sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("azure: connection test failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("azure: connection test returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// enumerationResultBlobs mirrors the XML envelope returned by the Azure
// List Blobs API.
type enumerationResultBlobs struct {
	XMLName    xml.Name    `xml:"EnumerationResults"`
	Blobs      []blobEntry `xml:"Blobs>Blob"`
	NextMarker string      `xml:"NextMarker"`
}

type blobEntry struct {
	Name       string         `xml:"Name"`
	Properties blobProperties `xml:"Properties"`
}

type blobProperties struct {
	LastModified  string `xml:"Last-Modified"`
	ContentLength int64  `xml:"Content-Length"`
}

// ListStateFiles enumerates all blobs in the container whose names start with
// the configured prefix and end with ".tfstate".  It handles pagination
// transparently and returns the results sorted by key.
func (c *Client) ListStateFiles(ctx context.Context) ([]StateFileRef, error) {
	var refs []StateFileRef
	marker := ""

	for {
		params := url.Values{
			"restype": {"container"},
			"comp":    {"list"},
		}
		if c.config.Prefix != "" {
			params.Set("prefix", c.config.Prefix)
		}
		if marker != "" {
			params.Set("marker", marker)
		}

		reqURL := fmt.Sprintf("%s?%s", c.containerURL(), params.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: failed to create list request: %w", err)
		}

		if err := c.signRequest(req); err != nil {
			return nil, fmt.Errorf("azure: failed to sign list request: %w", err)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("azure: list blobs request failed: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("azure: failed to read list response: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("azure: list blobs returned status %d: %s", resp.StatusCode, string(body))
		}

		var result enumerationResultBlobs
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("azure: failed to parse list response XML: %w", err)
		}

		for _, blob := range result.Blobs {
			if !strings.HasSuffix(blob.Name, ".tfstate") {
				continue
			}
			lastMod, _ := time.Parse(time.RFC1123, blob.Properties.LastModified)
			refs = append(refs, StateFileRef{
				Key:          blob.Name,
				LastModified: lastMod,
				Size:         blob.Properties.ContentLength,
			})
		}

		if result.NextMarker == "" {
			break
		}
		marker = result.NextMarker
	}

	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Key < refs[j].Key
	})

	return refs, nil
}

// DownloadState retrieves the raw content of the blob referenced by ref.
func (c *Client) DownloadState(ctx context.Context, ref StateFileRef) ([]byte, error) {
	reqURL := c.blobURL(ref.Key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: failed to create download request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("azure: failed to sign download request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure: download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("azure: download returned status %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("azure: failed to read blob content: %w", err)
	}

	return data, nil
}

// signRequest adds the required Azure shared-key authorization headers.
//
// This is a simplified implementation of the Azure Storage shared-key
// authorization scheme.  It covers the most common scenarios for blob
// operations (GET, HEAD, PUT) but does not implement the full canonical-header
// or canonical-resource specification.
func (c *Client) signRequest(req *http.Request) error {
	now := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("x-ms-date", now)
	req.Header.Set("x-ms-version", "2020-10-02")

	// Build the string to sign.
	// See: https://learn.microsoft.com/en-us/rest/api/storageservices/authorize-with-shared-key
	contentLength := req.Header.Get("Content-Length")
	if contentLength == "" || contentLength == "0" {
		contentLength = ""
	}

	canonicalHeaders := fmt.Sprintf("x-ms-date:%s\nx-ms-version:2020-10-02", now)
	canonicalResource := c.buildCanonicalResource(req)

	stringToSign := strings.Join([]string{
		req.Method,                         // HTTP verb
		req.Header.Get("Content-Encoding"), // Content-Encoding
		req.Header.Get("Content-Language"), // Content-Language
		contentLength,                      // Content-Length
		req.Header.Get("Content-MD5"),      // Content-MD5
		req.Header.Get("Content-Type"),     // Content-Type
		"",                                 // Date (empty because we use x-ms-date)
		"",                                 // If-Modified-Since
		"",                                 // If-Match
		"",                                 // If-None-Match
		"",                                 // If-Unmodified-Since
		"",                                 // Range
		canonicalHeaders,
		canonicalResource,
	}, "\n")

	keyBytes, err := base64.StdEncoding.DecodeString(c.config.Key)
	if err != nil {
		return fmt.Errorf("azure: failed to decode account key: %w", err)
	}

	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(stringToSign))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req.Header.Set("Authorization", fmt.Sprintf("SharedKey %s:%s", c.config.AccountName, signature))
	return nil
}

// buildCanonicalResource constructs the canonical resource string required by
// the shared-key signature.  It includes the account name, the URL path, and
// all query parameters in sorted order.
func (c *Client) buildCanonicalResource(req *http.Request) string {
	cr := fmt.Sprintf("/%s%s", c.config.AccountName, req.URL.Path)

	params := req.URL.Query()
	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, strings.ToLower(k))
		}
		sort.Strings(keys)

		for _, k := range keys {
			vals := params[k]
			sort.Strings(vals)
			cr += fmt.Sprintf("\n%s:%s", k, strings.Join(vals, ","))
		}
	}

	return cr
}
