package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	tshttp "github.com/terraform-state-manager/terraform-state-manager/internal/clients/http"
)

const (
	// defaultARMBaseURL is the Azure Resource Manager public-cloud endpoint.
	defaultARMBaseURL = "https://management.azure.com"

	// fallbackAPIVersion is used for resource types not present in the
	// apiVersions map. It is a recent, widely supported ARM API version; an
	// unknown type still gets a best-effort GET rather than being skipped.
	fallbackAPIVersion = "2021-04-01"

	defaultRateLimitDelay    = 0.1
	defaultConcurrentWorkers = 5
)

// apiVersions maps a namespace-qualified ARM resource type (lower-cased) to the
// API version used for its GET request. ARM requires an api-version query
// parameter and each resource type pins its own. This map covers the common
// azurerm resource types; anything missing falls back to fallbackAPIVersion.
var apiVersions = map[string]string{
	"microsoft.network/virtualnetworks":          "2023-09-01",
	"microsoft.network/virtualnetworks/subnets":  "2023-09-01",
	"microsoft.network/networksecuritygroups":    "2023-09-01",
	"microsoft.network/publicipaddresses":        "2023-09-01",
	"microsoft.compute/virtualmachines":          "2023-09-01",
	"microsoft.storage/storageaccounts":          "2023-01-01",
	"microsoft.keyvault/vaults":                  "2023-07-01",
	"microsoft.resources/resourcegroups":         "2021-04-01",
	"microsoft.containerservice/managedclusters": "2024-01-01",
	"microsoft.web/sites":                        "2023-12-01",
	"microsoft.dbforpostgresql/flexibleservers":  "2023-06-01-preview",
}

// apiVersionFor returns the ARM API version for a namespace-qualified resource
// type, falling back to fallbackAPIVersion when the type is not known.
func apiVersionFor(fullType string) string {
	if v, ok := apiVersions[strings.ToLower(fullType)]; ok {
		return v
	}
	return fallbackAPIVersion
}

// Config configures the live ARM ResourceReader.
//
// Credential supplies the bearer token; how the token is acquired (service
// principal, workload identity, managed identity) is out of scope for this
// package. BaseURL defaults to the public-cloud ARM endpoint and can be
// overridden for sovereign clouds or, in tests, an httptest server.
type Config struct {
	// Credential is the ARM bearer-token source. Required.
	Credential Credential
	// BaseURL overrides the ARM endpoint. Defaults to defaultARMBaseURL.
	BaseURL string
	// RateLimitDelay is the minimum delay between requests, in seconds.
	RateLimitDelay float64
}

// armReader is the live ResourceReader. It issues read-only ARM GETs via the
// shared HTTP client, attaching a freshly fetched bearer token per request.
type armReader struct {
	httpClient *tshttp.Client
	cred       Credential
	baseURL    string
}

// NewARMReader constructs a live ARM ResourceReader from cfg. It returns an
// error if no Credential is supplied. The returned reader performs only GETs.
func NewARMReader(cfg Config) (ResourceReader, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("azure: Credential is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultARMBaseURL
	}
	if cfg.RateLimitDelay == 0 {
		cfg.RateLimitDelay = defaultRateLimitDelay
	}

	requestsPerSecond := 1.0 / cfg.RateLimitDelay
	rateLimiter := tshttp.NewRateLimiter(requestsPerSecond, defaultConcurrentWorkers)

	httpClient := tshttp.NewClient(tshttp.ClientConfig{
		BaseURL:      cfg.BaseURL,
		Headers:      map[string]string{"Content-Type": "application/json"},
		MaxRetries:   3,
		RetryDelay:   1 * time.Second,
		Timeout:      30 * time.Second,
		MaxIdleConns: defaultConcurrentWorkers,
		RateLimiter:  rateLimiter,
	})

	return &armReader{
		httpClient: httpClient,
		cred:       cfg.Credential,
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
	}, nil
}

// armResource is the subset of an ARM resource representation we decode from a
// 200 response. Only the comparable key properties are retained.
type armResource struct {
	Location   string          `json:"location"`
	Kind       string          `json:"kind"`
	SKU        *armSKU         `json:"sku"`
	Properties json.RawMessage `json:"properties"`
}

type armSKU struct {
	Name string `json:"name"`
	Tier string `json:"tier"`
}

// ReadResource issues an ARM GET for armID and classifies the outcome. A 200
// yields ExistencePresent with extracted key properties; a 404 yields
// ExistenceMissing; an unavailable credential, unparseable ID, or access-denied
// yields ExistenceUnknown. A non-nil error is returned only for unexpected
// transport failures so the caller may retry or abort the whole run.
func (r *armReader) ReadResource(ctx context.Context, armID string) (ResourceState, error) {
	parsed, err := ParseResourceID(armID)
	if err != nil {
		return ResourceState{ID: armID, Existence: ExistenceUnknown, Note: "unparseable id"}, nil
	}

	token, err := r.cred.Token(ctx)
	if err != nil {
		// No credential configured is an expected, non-fatal condition.
		return ResourceState{ID: armID, Existence: ExistenceUnknown, Note: "credential unavailable"}, nil
	}

	params := url.Values{}
	params.Set("api-version", apiVersionFor(parsed.FullType()))

	// ARM resource IDs are already absolute paths under the management endpoint.
	// We build the full URL ourselves because the shared client's Do method does
	// not prepend BaseURL (only its Get/Post helpers do), and we need Do to use
	// our per-request Authorization header.
	fullURL := r.baseURL + "/" + strings.TrimLeft(parsed.Raw, "/") + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return ResourceState{}, fmt.Errorf("azure: building request for %s: %w", armID, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := r.httpClient.Do(ctx, req)
	if err != nil {
		return ResourceState{}, fmt.Errorf("azure: GET %s: %w", armID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		return decodePresent(armID, resp.Body)
	case resp.StatusCode == http.StatusNotFound:
		return ResourceState{ID: armID, Existence: ExistenceMissing}, nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return ResourceState{ID: armID, Existence: ExistenceUnknown, Note: "access denied"}, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ResourceState{}, fmt.Errorf("azure: GET %s: unexpected status %d: %s", armID, resp.StatusCode, string(body))
	}
}

// decodePresent extracts the comparable key properties from a 200 ARM response.
func decodePresent(armID string, body io.Reader) (ResourceState, error) {
	var res armResource
	if err := json.NewDecoder(body).Decode(&res); err != nil {
		return ResourceState{}, fmt.Errorf("azure: decoding response for %s: %w", armID, err)
	}
	return ResourceState{
		ID:         armID,
		Existence:  ExistencePresent,
		Properties: ExtractKeyProperties(res.Location, res.Kind, skuName(res.SKU), res.Properties),
	}, nil
}

func skuName(s *armSKU) string {
	if s == nil {
		return ""
	}
	return s.Name
}
