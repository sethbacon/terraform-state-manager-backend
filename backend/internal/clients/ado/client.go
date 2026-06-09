// Package ado provides a read-only Azure DevOps REST client and a dry-run
// migration plan enumerator. It is credential-agnostic: callers supply an
// opaque personal access token (PAT) and an organization base URL. The client
// performs only GET requests and never reads secret variable values.
package ado

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	tshttp "github.com/terraform-state-manager/terraform-state-manager/internal/clients/http"
)

const (
	// defaultAPIVersion is the Azure DevOps REST API version targeted by all
	// list calls. It is sent as the api-version query parameter.
	defaultAPIVersion = "7.1"

	defaultRateLimitDelay    = 0.1
	defaultConcurrentWorkers = 5
)

// Config holds the configuration for the Azure DevOps REST client.
//
// Token is an opaque personal access token. It is never logged and never
// persisted by this package; how it is obtained, stored, or encrypted is out
// of scope here. Token may be empty, in which case requests are sent without
// an Authorization header (useful for enumeration against a mock server).
type Config struct {
	OrganizationURL string
	Project         string
	Token           string
	RateLimitDelay  float64 // seconds between requests
}

// Client provides read-only access to a single Azure DevOps project. It wraps
// the shared HTTP client with ADO-specific configuration: PAT basic auth and a
// default api-version query parameter.
type Client struct {
	httpClient *tshttp.Client
	config     Config
}

// NewClient creates a new Azure DevOps REST client. OrganizationURL and Project
// must be non-empty. Token may be empty. Unset config fields receive sensible
// defaults and a rate limiter is configured from RateLimitDelay.
//
// PAT authentication uses HTTP Basic auth with an empty username and the token
// as the password, i.e. Authorization: Basic base64(":" + token). The token is
// injected via the shared client's Headers map; BearerToken is left empty.
func NewClient(cfg Config) (*Client, error) {
	if cfg.OrganizationURL == "" {
		return nil, fmt.Errorf("ado: OrganizationURL is required")
	}
	if cfg.Project == "" {
		return nil, fmt.Errorf("ado: Project is required")
	}

	if cfg.RateLimitDelay == 0 {
		cfg.RateLimitDelay = defaultRateLimitDelay
	}

	// Azure DevOps expects application/json bodies; override the shared client's
	// HCP-oriented default Content-Type. The shared client applies config.Headers
	// last, so this wins over the built-in default for every request.
	headers := map[string]string{
		"Content-Type": "application/json",
	}
	if cfg.Token != "" {
		// PAT scheme: basic auth with empty username, token as password.
		encoded := base64.StdEncoding.EncodeToString([]byte(":" + cfg.Token))
		headers["Authorization"] = "Basic " + encoded
	}

	requestsPerSecond := 1.0 / cfg.RateLimitDelay
	rateLimiter := tshttp.NewRateLimiter(requestsPerSecond, defaultConcurrentWorkers)

	httpClient := tshttp.NewClient(tshttp.ClientConfig{
		BaseURL:      cfg.OrganizationURL,
		Headers:      headers,
		MaxRetries:   3,
		RetryDelay:   1 * time.Second,
		Timeout:      30 * time.Second,
		MaxIdleConns: defaultConcurrentWorkers,
		RateLimiter:  rateLimiter,
	})

	return &Client{
		httpClient: httpClient,
		config:     cfg,
	}, nil
}

// projectPath joins the configured project with the given API sub-path, e.g.
// projectPath("_apis/git/repositories") => "{project}/_apis/git/repositories".
// The OrganizationURL (base) is prepended by the shared HTTP client.
func (c *Client) projectPath(apiPath string) string {
	return url.PathEscape(c.config.Project) + "/" + apiPath
}

// defaultParams returns the query parameters applied to every list request,
// currently just the api-version. Callers may add to the returned url.Values.
func defaultParams() url.Values {
	params := url.Values{}
	params.Set("api-version", defaultAPIVersion)
	return params
}

// listEnvelope is the standard Azure DevOps collection response shape:
// { "count": <n>, "value": [ ... ] }.
type listEnvelope[T any] struct {
	Count int `json:"count"`
	Value []T `json:"value"`
}

// APIError describes a non-2xx response from the Azure DevOps REST API. It
// carries the HTTP status code and response body so callers can distinguish
// recoverable conditions (such as a 409 Conflict signalling a resource that
// already exists) from genuine failures.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ado: unexpected status %d: %s", e.StatusCode, e.Body)
}

// IsConflict reports whether err is an APIError carrying HTTP 409 Conflict,
// which Azure DevOps returns when a create call targets a resource that already
// exists. The execute orchestrator treats this as an idempotent success.
func IsConflict(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusConflict
}

// postJSON performs a POST to the given project-relative path with a JSON body
// and decodes a successful (2xx) response into target. Non-2xx responses are
// returned as an *APIError so callers can inspect the status code (e.g. via
// IsConflict). A nil target skips response decoding.
func (c *Client) postJSON(ctx context.Context, path string, params url.Values, body, target any) error {
	fullPath := path
	if len(params) > 0 {
		fullPath = path + "?" + params.Encode()
	}

	resp, err := c.httpClient.Post(ctx, fullPath, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &APIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}

	if target == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decoding response from %s: %w", path, err)
	}
	return nil
}
