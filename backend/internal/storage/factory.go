package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage/azure"
	gcsstorage "github.com/terraform-state-manager/terraform-state-manager/internal/storage/gcs"
	"github.com/terraform-state-manager/terraform-state-manager/internal/storage/local"
	s3storage "github.com/terraform-state-manager/terraform-state-manager/internal/storage/s3"
)

// NewBackend creates a storage Backend based on the provided configuration.
func NewBackend(cfg *config.StorageConfig) (Backend, error) {
	switch cfg.DefaultBackend {
	case "local":
		return local.NewBackend(cfg.Local.BasePath)
	case "s3":
		s3Cfg := s3storage.Config{
			Bucket:          cfg.S3.Bucket,
			Region:          cfg.S3.Region,
			AccessKeyID:     cfg.S3.AccessKeyID,
			SecretAccessKey: cfg.S3.SecretAccessKey,
			Endpoint:        cfg.S3.Endpoint,
		}
		return s3storage.New(context.Background(), s3Cfg)
	case "gcs":
		return gcsstorage.New(context.Background(), cfg.GCS)
	case "azure":
		azureCfg := azure.Config{
			AccountName:   cfg.Azure.AccountName,
			AccountKey:    cfg.Azure.AccountKey,
			ContainerName: cfg.Azure.ContainerName,
		}
		return azure.New(context.Background(), azureCfg)
	default:
		return nil, fmt.Errorf("unsupported storage backend: %s", cfg.DefaultBackend)
	}
}

// NewBackendFromRawConfig creates a storage Backend from a backend type string
// and the raw JSON configuration carried by a migration job's source_config or
// target_config.
//
// Unlike NewBackend, which builds every backend from the single global server
// StorageConfig, this decodes the per-job connection details so that distinct
// jobs (or the source and target of one job) targeting the same backend type
// resolve independently — e.g. two different local base paths, or two different
// S3 buckets. JSON keys follow the snake_case convention used by the state
// source clients.
func NewBackendFromRawConfig(backendType string, raw json.RawMessage) (Backend, error) {
	switch backendType {
	case "local":
		var c struct {
			BasePath string `json:"base_path"`
		}
		if err := decodeStorageConfig(raw, &c); err != nil {
			return nil, err
		}
		return local.NewBackend(c.BasePath)
	case "s3":
		var c struct {
			Bucket          string `json:"bucket"`
			Region          string `json:"region"`
			Prefix          string `json:"prefix"`
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			Endpoint        string `json:"endpoint"`
			ForcePathStyle  bool   `json:"force_path_style"`
		}
		if err := decodeStorageConfig(raw, &c); err != nil {
			return nil, err
		}
		return s3storage.New(context.Background(), s3storage.Config{
			Bucket:          c.Bucket,
			Region:          c.Region,
			Prefix:          c.Prefix,
			AccessKeyID:     c.AccessKeyID,
			SecretAccessKey: c.SecretAccessKey,
			Endpoint:        c.Endpoint,
			ForcePathStyle:  c.ForcePathStyle,
		})
	case "gcs":
		var c struct {
			Bucket          string `json:"bucket"`
			ProjectID       string `json:"project_id"`
			CredentialsFile string `json:"credentials_file"`
			CredentialsJSON string `json:"credentials_json"`
			Endpoint        string `json:"endpoint"`
		}
		if err := decodeStorageConfig(raw, &c); err != nil {
			return nil, err
		}
		return gcsstorage.New(context.Background(), config.GCSStorageConfig{
			Bucket:          c.Bucket,
			ProjectID:       c.ProjectID,
			CredentialsFile: c.CredentialsFile,
			CredentialsJSON: c.CredentialsJSON,
			Endpoint:        c.Endpoint,
		})
	case "azure":
		var c struct {
			AccountName   string `json:"account_name"`
			AccountKey    string `json:"account_key"`
			ContainerName string `json:"container_name"`
			Prefix        string `json:"prefix"`
			Endpoint      string `json:"endpoint"`
		}
		if err := decodeStorageConfig(raw, &c); err != nil {
			return nil, err
		}
		return azure.New(context.Background(), azure.Config{
			AccountName:   c.AccountName,
			AccountKey:    c.AccountKey,
			ContainerName: c.ContainerName,
			Prefix:        c.Prefix,
			Endpoint:      c.Endpoint,
		})
	default:
		return nil, fmt.Errorf("unsupported storage backend: %s", backendType)
	}
}

// decodeStorageConfig unmarshals raw JSON config into v. An empty payload is
// treated as an empty config, leaving required-field validation to the concrete
// storage constructor.
func decodeStorageConfig(raw json.RawMessage, v interface{}) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid storage config JSON: %w", err)
	}
	return nil
}
