package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ClientConfig holds all configuration for the shared HTTP client.
type ClientConfig struct {
	BaseURL      string
	BearerToken  string
	Headers      map[string]string
	MaxRetries   int
	RetryDelay   time.Duration
	Timeout      time.Duration
	MaxIdleConns int
	RateLimiter  *RateLimiter
}

// Client is a shared HTTP client with connection pooling, retries, and rate limiting.
type Client struct {
	httpClient  *http.Client
	config      ClientConfig
	rateLimiter *RateLimiter
}

// retryableStatusCodes defines HTTP status codes that should trigger a retry.
var retryableStatusCodes = map[int]bool{
	http.StatusTooManyRequests:     true, // 429
	http.StatusInternalServerError: true, // 500
	http.StatusBadGateway:          true, // 502
	http.StatusServiceUnavailable:  true, // 503
	http.StatusGatewayTimeout:      true, // 504
}

// NewClient creates a new HTTP client with connection pooling, retry logic,
// and optional rate limiting. Sensible defaults are applied for unset config fields.
func NewClient(cfg ClientConfig) *Client {
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 1 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxIdleConns == 0 {
		cfg.MaxIdleConns = 10
	}

	transport := &http.Transport{
		MaxIdleConns:        cfg.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.MaxIdleConns,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
		config:      cfg,
		rateLimiter: cfg.RateLimiter,
	}
}

// Get performs an HTTP GET request to the given path with optional query parameters.
// The path is appended to the client's BaseURL. The caller is responsible for
// closing the response body.
func (c *Client) Get(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	fullURL := c.buildURL(path)
	if len(params) > 0 {
		fullURL = fullURL + "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating GET request for %s: %w", fullURL, err)
	}

	return c.Do(ctx, req)
}

// Post performs an HTTP POST request to the given path. The body is serialized
// to JSON. The caller is responsible for closing the response body.
func (c *Client) Post(ctx context.Context, path string, body interface{}) (*http.Response, error) {
	fullURL := c.buildURL(path)

	var bodyReader io.Reader
	if body != nil {
		jsonBytes, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling POST body for %s: %w", fullURL, err)
		}
		bodyReader = bytes.NewReader(jsonBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating POST request for %s: %w", fullURL, err)
	}

	return c.Do(ctx, req)
}

// Do executes the given HTTP request with rate limiting, automatic header injection,
// and retry logic with exponential backoff. Retries are attempted for transient
// server errors (500, 502, 503, 504) and rate limit responses (429).
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	// Apply default headers
	c.applyHeaders(req)

	var lastErr error
	var lastResp *http.Response

	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		// Wait for rate limiter before each attempt
		if c.rateLimiter != nil {
			if err := c.rateLimiter.Wait(ctx); err != nil {
				return nil, fmt.Errorf("rate limiter wait: %w", err)
			}
		}

		// Clone the request body for retries (the original body may have been consumed)
		retryReq := req
		if attempt > 0 && req.Body != nil {
			retryReq = req.Clone(ctx)
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("getting request body for retry: %w", err)
				}
				retryReq.Body = body
			}
		}

		resp, err := c.httpClient.Do(retryReq)
		if err != nil {
			lastErr = fmt.Errorf("executing request (attempt %d/%d): %w", attempt+1, c.config.MaxRetries+1, err)
			if attempt < c.config.MaxRetries {
				sleepDuration := c.backoffDelay(attempt)
				if err := sleepWithContext(ctx, sleepDuration); err != nil {
					return nil, fmt.Errorf("context cancelled during retry backoff: %w", err)
				}
			}
			continue
		}

		// Check if we should retry this status code
		if !retryableStatusCodes[resp.StatusCode] {
			return resp, nil
		}

		// Determine retry delay: use Retry-After header if present, otherwise exponential backoff
		retryDelay := c.retryDelay(resp, attempt)

		// Close the response body before retrying to free the connection
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		lastResp = resp
		lastErr = fmt.Errorf("received retryable status %d (attempt %d/%d)", resp.StatusCode, attempt+1, c.config.MaxRetries+1)

		if attempt < c.config.MaxRetries {
			if err := sleepWithContext(ctx, retryDelay); err != nil {
				return nil, fmt.Errorf("context cancelled during retry backoff: %w", err)
			}
		}
	}

	if lastResp != nil {
		return nil, fmt.Errorf("all %d retries exhausted, last status: %d: %w", c.config.MaxRetries+1, lastResp.StatusCode, lastErr)
	}
	return nil, fmt.Errorf("all %d retries exhausted: %w", c.config.MaxRetries+1, lastErr)
}

// GetJSON performs a GET request and unmarshals the JSON response body into the
// provided target. The response body is always closed before returning.
func (c *Client) GetJSON(ctx context.Context, path string, params url.Values, target interface{}) error {
	resp, err := c.Get(ctx, path, params)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d for GET %s: %s", resp.StatusCode, path, string(bodyBytes))
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decoding JSON response from %s: %w", path, err)
	}

	return nil
}

// buildURL joins the base URL and the provided path.
func (c *Client) buildURL(path string) string {
	base := strings.TrimRight(c.config.BaseURL, "/")
	path = strings.TrimLeft(path, "/")
	if path == "" {
		return base
	}
	return base + "/" + path
}

// applyHeaders sets default headers on the request including authorization,
// content type, and any custom headers from the client config.
func (c *Client) applyHeaders(req *http.Request) {
	if c.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.BearerToken)
	}

	// Set Content-Type for HCP Terraform API compatibility
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/vnd.api+json")
	}

	for key, value := range c.config.Headers {
		req.Header.Set(key, value)
	}
}

// backoffDelay returns the exponential backoff duration for the given attempt number.
func (c *Client) backoffDelay(attempt int) time.Duration {
	backoff := float64(c.config.RetryDelay) * math.Pow(2, float64(attempt))
	return time.Duration(backoff)
}

// retryDelay determines how long to wait before retrying. It respects the
// Retry-After header when present, falling back to exponential backoff.
func (c *Client) retryDelay(resp *http.Response, attempt int) time.Duration {
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		// Try parsing as seconds first
		if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil {
			return time.Duration(seconds) * time.Second
		}
		// Try parsing as HTTP-date
		if t, err := http.ParseTime(retryAfter); err == nil {
			delay := time.Until(t)
			if delay > 0 {
				return delay
			}
		}
	}
	return c.backoffDelay(attempt)
}

// sleepWithContext sleeps for the given duration unless the context is cancelled.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
