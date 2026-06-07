// Package ado provides a read-only Azure DevOps REST client and a dry-run
// migration plan enumerator. It is credential-agnostic: callers supply an
// opaque personal access token (PAT) and an organization base URL. The client
// performs only GET requests and never reads secret variable values.
package ado

import (
	"encoding/base64"
	"fmt"
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

	headers := map[string]string{}
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
