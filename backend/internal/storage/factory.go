package storage

import (
	"context"
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
