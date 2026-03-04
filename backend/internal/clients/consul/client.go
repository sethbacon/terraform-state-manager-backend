// Package consul implements a Terraform state file scanner for HashiCorp Consul
// KV store.
//
// Terraform's consul backend stores state under a configurable KV path.  This
// client uses the Consul HTTP API to enumerate and download those state files.
package consul

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/azure"
)

// Config holds the parameters needed to connect to a Consul agent or cluster.
type Config struct {
	// Address is the Consul HTTP address (e.g. "127.0.0.1:8500").
	Address string `json:"address"`
	// Scheme is "http" or "https".
	Scheme string `json:"scheme"`
	// Datacenter targets a specific Consul datacenter.
	Datacenter string `json:"datacenter,omitempty"`
	// Token is the Consul ACL token for authentication.
	Token string `json:"token,omitempty"`
	// Path is the KV prefix under which Terraform state files are stored.
	Path string `json:"path,omitempty"`
}

// Client communicates with Consul through its HTTP API.
type Client struct {
	config     Config
	httpClient *http.Client
}

// NewClient validates the supplied configuration and returns a ready-to-use
// Client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("consul: address is required")
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "http"
	}
	if cfg.Scheme != "http" && cfg.Scheme != "https" {
		return nil, fmt.Errorf("consul: scheme must be 'http' or 'https', got %q", cfg.Scheme)
	}

	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// baseURL returns the Consul API base URL.
func (c *Client) baseURL() string {
	return fmt.Sprintf("%s://%s", c.config.Scheme, c.config.Address)
}

// newRequest creates an HTTP request with common headers (ACL token,
// datacenter query parameter).
func (c *Client) newRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}

	if c.config.Token != "" {
		req.Header.Set("X-Consul-Token", c.config.Token)
	}

	if c.config.Datacenter != "" {
		q := req.URL.Query()
		q.Set("dc", c.config.Datacenter)
		req.URL.RawQuery = q.Encode()
	}

	return req, nil
}

// TestConnection verifies connectivity by hitting the Consul agent self
// endpoint.
func (c *Client) TestConnection(ctx context.Context) error {
	reqURL := fmt.Sprintf("%s/v1/agent/self", c.baseURL())
	req, err := c.newRequest(ctx, http.MethodGet, reqURL)
	if err != nil {
		return fmt.Errorf("consul: failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("consul: connection test failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("consul: connection test returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// ListStateFiles enumerates all KV keys under the configured path and returns
// those that look like Terraform state files (keys ending with ".tfstate", or
// any key if no suffix filter applies since Consul backends often store state
// without extensions).
func (c *Client) ListStateFiles(ctx context.Context) ([]azure.StateFileRef, error) {
	path := strings.TrimRight(c.config.Path, "/")
	if path == "" {
		path = "terraform"
	}

	reqURL := fmt.Sprintf("%s/v1/kv/%s?keys=true&separator=", c.baseURL(), url.PathEscape(path))
	req, err := c.newRequest(ctx, http.MethodGet, reqURL)
	if err != nil {
		return nil, fmt.Errorf("consul: failed to create list request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("consul: list keys request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A 404 means the path does not exist -- treat as empty.
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("consul: failed to read list response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("consul: list keys returned status %d: %s", resp.StatusCode, string(body))
	}

	var keys []string
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil, fmt.Errorf("consul: failed to parse key list: %w", err)
	}

	// For each key, fetch metadata via a non-raw GET so we can read
	// ModifyIndex and the value length.
	var refs []azure.StateFileRef
	for _, key := range keys {
		ref, err := c.fetchKeyMetadata(ctx, key)
		if err != nil {
			// Log and skip keys we cannot inspect.
			continue
		}
		refs = append(refs, ref)
	}

	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Key < refs[j].Key
	})

	return refs, nil
}

// consulKVEntry represents a single Consul KV entry as returned by the
// non-raw GET endpoint.
type consulKVEntry struct {
	Key         string `json:"Key"`
	Value       string `json:"Value"` // base64-encoded
	ModifyIndex int64  `json:"ModifyIndex"`
}

// fetchKeyMetadata retrieves the metadata for a single KV key without
// downloading the full value.
func (c *Client) fetchKeyMetadata(ctx context.Context, key string) (azure.StateFileRef, error) {
	reqURL := fmt.Sprintf("%s/v1/kv/%s", c.baseURL(), url.PathEscape(key))
	req, err := c.newRequest(ctx, http.MethodGet, reqURL)
	if err != nil {
		return azure.StateFileRef{}, fmt.Errorf("consul: failed to create metadata request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return azure.StateFileRef{}, fmt.Errorf("consul: metadata request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return azure.StateFileRef{}, fmt.Errorf("consul: failed to read metadata response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return azure.StateFileRef{}, fmt.Errorf("consul: metadata returned status %d", resp.StatusCode)
	}

	var entries []consulKVEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return azure.StateFileRef{}, fmt.Errorf("consul: failed to parse metadata: %w", err)
	}

	if len(entries) == 0 {
		return azure.StateFileRef{}, fmt.Errorf("consul: no entry found for key %q", key)
	}

	entry := entries[0]

	// Consul does not expose a real timestamp on KV entries so we use the
	// current time as a best-effort indicator.  The ModifyIndex can be
	// used for ordering instead.
	return azure.StateFileRef{
		Key:          entry.Key,
		LastModified: time.Now().UTC(),
		Size:         int64(len(entry.Value)),
	}, nil
}

// DownloadState retrieves the raw value of the KV key referenced by ref.
func (c *Client) DownloadState(ctx context.Context, ref azure.StateFileRef) ([]byte, error) {
	reqURL := fmt.Sprintf("%s/v1/kv/%s?raw=true", c.baseURL(), url.PathEscape(ref.Key))
	req, err := c.newRequest(ctx, http.MethodGet, reqURL)
	if err != nil {
		return nil, fmt.Errorf("consul: failed to create download request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("consul: download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("consul: download returned status %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("consul: failed to read KV value: %w", err)
	}

	return data, nil
}
