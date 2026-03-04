// Package http_backend implements a Terraform state file scanner for the
// standard Terraform HTTP backend.
//
// The HTTP backend stores state at a single HTTP(S) URL (the "address" field
// in the backend configuration).  GET retrieves the state, POST updates it,
// and LOCK/UNLOCK manage advisory locks.  Because the backend typically serves
// exactly one state file, ListStateFiles always returns at most one entry.
package http_backend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/azure"
)

// Config holds the parameters needed to connect to a Terraform HTTP backend.
type Config struct {
	// Address is the URL used to GET the Terraform state.
	Address string `json:"address"`
	// LockAddress is the URL used to LOCK the state (optional).
	LockAddress string `json:"lock_address,omitempty"`
	// UnlockAddress is the URL used to UNLOCK the state (optional).
	UnlockAddress string `json:"unlock_address,omitempty"`
	// Username is used for HTTP basic authentication (optional).
	Username string `json:"username,omitempty"`
	// Password is the HTTP basic auth password (optional).
	Password string `json:"password,omitempty"`
}

// Client communicates with a Terraform HTTP backend.
type Client struct {
	config     Config
	httpClient *http.Client
}

// NewClient validates the supplied configuration and returns a ready-to-use
// Client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("http_backend: address is required")
	}

	return &Client{
		config: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// setAuth adds HTTP Basic authentication to the request if credentials are
// configured.
func (c *Client) setAuth(req *http.Request) {
	if c.config.Username != "" || c.config.Password != "" {
		req.SetBasicAuth(c.config.Username, c.config.Password)
	}
}

// TestConnection verifies that the HTTP backend is reachable and responds
// successfully.  It sends a HEAD request to the address URL and expects a
// 200 OK or 204 No Content response.
func (c *Client) TestConnection(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.config.Address, nil)
	if err != nil {
		return fmt.Errorf("http_backend: failed to create request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http_backend: connection test failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Accept 200, 204, and 404 (no state yet) as valid responses -- they all
	// indicate the backend is reachable.
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("http_backend: connection test returned status %d: %s", resp.StatusCode, string(body))
	}
}

// ListStateFiles returns at most one StateFileRef for the configured HTTP
// backend address.  The HTTP backend model is one-URL-per-state, so
// enumeration is limited to probing whether the endpoint currently holds a
// state file.
func (c *Client) ListStateFiles(ctx context.Context) ([]azure.StateFileRef, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.config.Address, nil)
	if err != nil {
		return nil, fmt.Errorf("http_backend: failed to create HEAD request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_backend: HEAD request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// No state exists yet.
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("http_backend: HEAD returned status %d: %s", resp.StatusCode, string(body))
	}

	lastMod := time.Time{}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if parsed, err := http.ParseTime(lm); err == nil {
			lastMod = parsed
		}
	}

	ref := azure.StateFileRef{
		Key:          c.config.Address,
		LastModified: lastMod,
		Size:         resp.ContentLength,
	}

	return []azure.StateFileRef{ref}, nil
}

// DownloadState retrieves the raw Terraform state from the HTTP backend.
func (c *Client) DownloadState(ctx context.Context, ref azure.StateFileRef) ([]byte, error) {
	// Always use the configured address regardless of what ref.Key contains.
	reqURL := c.config.Address
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("http_backend: failed to create download request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_backend: download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("http_backend: download returned status %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http_backend: failed to read response body: %w", err)
	}

	return data, nil
}
