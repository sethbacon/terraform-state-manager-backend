package analyzer

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	tshttp "github.com/terraform-state-manager/terraform-state-manager/internal/clients/http"
)

// defaultRegistryBaseURL is the public Terraform Registry provider-versions API.
// No credentials are required for these endpoints.
const defaultRegistryBaseURL = "https://registry.terraform.io"

// ProviderVersionSource resolves the latest published version of a Terraform
// provider. It is the seam that lets pin-drift analysis run against the live
// public registry in production and against recorded fixtures in tests — CI
// never makes outbound HTTP calls.
type ProviderVersionSource interface {
	// LatestVersion returns the newest published, non-prerelease version of the
	// provider identified by namespace/type (e.g. "hashicorp", "aws").
	//
	// It returns an empty string with a nil error when the provider exists but
	// has no usable (stable) version. A non-nil error signals the version could
	// not be determined at all (network failure, unknown provider, malformed
	// response) — callers treat that as "unknown", never as a hard failure.
	LatestVersion(ctx context.Context, namespace, providerType string) (string, error)
}

// RegistryProviderVersionSource is the live ProviderVersionSource backed by the
// public Terraform Registry. It wraps the shared HTTP client (connection
// pooling + retries) used elsewhere in the codebase.
type RegistryProviderVersionSource struct {
	httpClient *tshttp.Client
}

// registryVersionsResponse models the subset of the
// GET /v1/providers/{namespace}/{type}/versions payload we need.
type registryVersionsResponse struct {
	Versions []struct {
		Version string `json:"version"`
	} `json:"versions"`
}

// NewRegistryProviderVersionSource builds a live registry source pointed at the
// public Terraform Registry. A zero-value baseURL falls back to the public host.
func NewRegistryProviderVersionSource(baseURL string) *RegistryProviderVersionSource {
	if baseURL == "" {
		baseURL = defaultRegistryBaseURL
	}
	httpClient := tshttp.NewClient(tshttp.ClientConfig{
		BaseURL:    baseURL,
		MaxRetries: 3,
		RetryDelay: 1 * time.Second,
		Timeout:    15 * time.Second,
	})
	return &RegistryProviderVersionSource{httpClient: httpClient}
}

// LatestVersion fetches the published versions for a provider from the registry
// and returns the newest non-prerelease one. See ProviderVersionSource.
func (s *RegistryProviderVersionSource) LatestVersion(ctx context.Context, namespace, providerType string) (string, error) {
	namespace = strings.TrimSpace(namespace)
	providerType = strings.TrimSpace(providerType)
	if namespace == "" || providerType == "" {
		return "", fmt.Errorf("provider namespace and type are required")
	}

	path := fmt.Sprintf("/v1/providers/%s/%s/versions",
		url.PathEscape(namespace), url.PathEscape(providerType))

	var payload registryVersionsResponse
	if err := s.httpClient.GetJSON(ctx, path, nil, &payload); err != nil {
		return "", fmt.Errorf("fetching provider versions for %s/%s: %w", namespace, providerType, err)
	}

	versions := make([]string, 0, len(payload.Versions))
	for _, v := range payload.Versions {
		versions = append(versions, v.Version)
	}
	return latestStableVersion(versions), nil
}

// StaticProviderVersionSource is an in-memory ProviderVersionSource for tests
// and offline operation. It serves versions from a recorded map keyed by the
// "namespace/type" provider address, with no network access.
type StaticProviderVersionSource struct {
	// Versions maps "namespace/type" (e.g. "hashicorp/aws") to the list of
	// published version strings, mirroring the registry response.
	Versions map[string][]string
	// Unknown, when true for a "namespace/type" key, forces LatestVersion to
	// return an error for that provider, simulating an unknown provider or a
	// network failure.
	Unknown map[string]bool
}

// LatestVersion resolves the newest non-prerelease version from the recorded
// map. Providers marked Unknown (or absent from the map) return an error so the
// caller records them as "unknown". See ProviderVersionSource.
func (s *StaticProviderVersionSource) LatestVersion(ctx context.Context, namespace, providerType string) (string, error) {
	key := namespace + "/" + providerType
	if s.Unknown[key] {
		return "", fmt.Errorf("provider %s is unknown", key)
	}
	versions, ok := s.Versions[key]
	if !ok {
		return "", fmt.Errorf("provider %s not found", key)
	}
	return latestStableVersion(versions), nil
}

// parseProviderSource splits a provider source address into its namespace and
// type. It accepts both fully-qualified addresses
// ("registry.terraform.io/hashicorp/aws") and short forms ("hashicorp/aws").
// ok is false when the address does not contain a namespace/type pair.
func parseProviderSource(source string) (namespace, providerType string, ok bool) {
	parts := strings.Split(strings.TrimSpace(source), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	// The last two segments are always namespace/type; any leading segment is
	// the registry host, which the public registry API does not need.
	namespace = parts[len(parts)-2]
	providerType = parts[len(parts)-1]
	if namespace == "" || providerType == "" {
		return "", "", false
	}
	return namespace, providerType, true
}

// latestStableVersion returns the highest non-prerelease version from a list,
// using the package's numeric version comparator. Prerelease versions (those
// carrying a "-" suffix such as "5.0.0-beta1") are skipped. An empty string is
// returned when no stable version is present.
func latestStableVersion(versions []string) string {
	latest := ""
	for _, v := range versions {
		v = strings.TrimSpace(v)
		if v == "" || strings.Contains(v, "-") {
			continue
		}
		if latest == "" || compareVersions(v, latest) > 0 {
			latest = v
		}
	}
	return latest
}
