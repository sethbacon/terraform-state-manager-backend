package hcp

import (
	"time"

	tshttp "github.com/terraform-state-manager/terraform-state-manager/internal/clients/http"
)

const (
	defaultBaseURL           = "https://app.terraform.io/api/v2"
	defaultConcurrentWorkers = 5
	defaultBatchSize         = 20
	defaultRateLimitDelay    = 0.1
)

// Config holds the configuration for the HCP Terraform API client.
type Config struct {
	Token             string
	BaseURL           string
	Organization      string
	ConcurrentWorkers int
	BatchSize         int
	RateLimitDelay    float64 // seconds between requests
}

// Client provides access to the HCP Terraform API. It wraps the shared HTTP
// client with HCP-specific configuration and JSONAPI response handling.
type Client struct {
	httpClient *tshttp.Client
	config     Config
}

// NewClient creates a new HCP Terraform API client. Unset config fields
// receive sensible defaults. A rate limiter is automatically configured
// based on the RateLimitDelay setting.
func NewClient(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.ConcurrentWorkers == 0 {
		cfg.ConcurrentWorkers = defaultConcurrentWorkers
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.RateLimitDelay == 0 {
		cfg.RateLimitDelay = defaultRateLimitDelay
	}

	// Calculate requests per second from the delay between requests
	requestsPerSecond := 1.0 / cfg.RateLimitDelay

	rateLimiter := tshttp.NewRateLimiter(requestsPerSecond, cfg.ConcurrentWorkers)

	httpClient := tshttp.NewClient(tshttp.ClientConfig{
		BaseURL:      cfg.BaseURL,
		BearerToken:  cfg.Token,
		MaxRetries:   3,
		RetryDelay:   1 * time.Second,
		Timeout:      30 * time.Second,
		MaxIdleConns: cfg.ConcurrentWorkers,
		RateLimiter:  rateLimiter,
	})

	return &Client{
		httpClient: httpClient,
		config:     cfg,
	}
}
