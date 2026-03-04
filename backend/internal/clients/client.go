// Package clients defines the StateClient interface implemented by all
// supported Terraform state backends and provides a factory for creating
// client instances from a source type and raw JSON configuration.
package clients

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/azure"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/consul"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/gcs"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/hcp"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/http_backend"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/k8s"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/pg"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/s3"
)

// StateClient is the common interface for testing connectivity to any
// state source backend. All backend-specific clients satisfy this interface.
type StateClient interface {
	// TestConnection verifies that the configuration and credentials are valid.
	TestConnection(ctx context.Context) error
}

// HCPClientAdapter wraps the HCP Terraform client, adding the TestConnection
// method required by the StateClient interface. The underlying *hcp.Client is
// promoted so callers can use GetWorkspaces, DownloadState, etc. directly.
type HCPClientAdapter struct {
	*hcp.Client
	Organization string
}

// TestConnection verifies the HCP Terraform API is reachable and the token is
// valid by calling the organizations endpoint and checking that the configured
// organization is accessible.
func (a *HCPClientAdapter) TestConnection(ctx context.Context) error {
	orgs, err := a.GetOrganizations(ctx)
	if err != nil {
		return fmt.Errorf("hcp: connection test failed: %w", err)
	}

	if a.Organization != "" {
		found := false
		for _, org := range orgs {
			if org.Name == a.Organization {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("hcp: organization %q not found or not accessible", a.Organization)
		}
	}

	return nil
}

// hcpConfigJSON maps the JSON config stored in the database to hcp.Config
// fields using explicit JSON struct tags.
type hcpConfigJSON struct {
	Token             string  `json:"token"`
	BaseURL           string  `json:"base_url"`
	Organization      string  `json:"organization"`
	ConcurrentWorkers int     `json:"concurrent_workers"`
	BatchSize         int     `json:"batch_size"`
	RateLimitDelay    float64 `json:"rate_limit_delay"`
}

// NewClientFromConfig creates a StateClient for the given source type by
// unmarshaling the raw JSON configuration into the backend-specific Config
// struct and constructing the appropriate client.
func NewClientFromConfig(sourceType string, rawConfig json.RawMessage) (StateClient, error) {
	switch sourceType {
	case "hcp_terraform":
		return newHCPClient(rawConfig)
	case "azure_blob":
		return newAzureClient(rawConfig)
	case "s3":
		return newS3Client(rawConfig)
	case "gcs":
		return newGCSClient(rawConfig)
	case "consul":
		return newConsulClient(rawConfig)
	case "pg":
		return newPGClient(rawConfig)
	case "kubernetes":
		return newK8sClient(rawConfig)
	case "http":
		return newHTTPBackendClient(rawConfig)
	default:
		return nil, fmt.Errorf("unsupported source type: %s", sourceType)
	}
}

func newHCPClient(rawConfig json.RawMessage) (*HCPClientAdapter, error) {
	var jcfg hcpConfigJSON
	if err := json.Unmarshal(rawConfig, &jcfg); err != nil {
		return nil, fmt.Errorf("invalid HCP config: %w", err)
	}
	if jcfg.Token == "" {
		return nil, fmt.Errorf("hcp: token is required")
	}
	if jcfg.Organization == "" {
		return nil, fmt.Errorf("hcp: organization is required")
	}

	cfg := hcp.Config{
		Token:             jcfg.Token,
		BaseURL:           jcfg.BaseURL,
		Organization:      jcfg.Organization,
		ConcurrentWorkers: jcfg.ConcurrentWorkers,
		BatchSize:         jcfg.BatchSize,
		RateLimitDelay:    jcfg.RateLimitDelay,
	}

	client := hcp.NewClient(cfg)
	return &HCPClientAdapter{Client: client, Organization: jcfg.Organization}, nil
}

func newAzureClient(rawConfig json.RawMessage) (*azure.Client, error) {
	var cfg azure.Config
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil, fmt.Errorf("invalid Azure config: %w", err)
	}
	return azure.NewClient(cfg)
}

func newS3Client(rawConfig json.RawMessage) (*s3.Client, error) {
	var cfg s3.Config
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil, fmt.Errorf("invalid S3 config: %w", err)
	}
	return s3.NewClient(cfg)
}

func newGCSClient(rawConfig json.RawMessage) (*gcs.Client, error) {
	var cfg gcs.Config
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil, fmt.Errorf("invalid GCS config: %w", err)
	}
	return gcs.NewClient(cfg)
}

func newConsulClient(rawConfig json.RawMessage) (*consul.Client, error) {
	var cfg consul.Config
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil, fmt.Errorf("invalid Consul config: %w", err)
	}
	return consul.NewClient(cfg)
}

func newPGClient(rawConfig json.RawMessage) (*pg.Client, error) {
	var cfg pg.Config
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL config: %w", err)
	}
	return pg.NewClient(cfg)
}

func newK8sClient(rawConfig json.RawMessage) (*k8s.Client, error) {
	var cfg k8s.Config
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil, fmt.Errorf("invalid Kubernetes config: %w", err)
	}
	return k8s.NewClient(cfg)
}

func newHTTPBackendClient(rawConfig json.RawMessage) (*http_backend.Client, error) {
	var cfg http_backend.Config
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil, fmt.Errorf("invalid HTTP backend config: %w", err)
	}
	return http_backend.NewClient(cfg)
}
