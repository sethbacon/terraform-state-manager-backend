// Package azure implements the storage.Backend interface using Azure Blob Storage.
package azure

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// Config holds the configuration for the Azure Blob Storage backend.
type Config struct {
	AccountName   string
	AccountKey    string
	ContainerName string
	Prefix        string // Optional key prefix (e.g. "terraform/states")
	Endpoint      string // Optional endpoint override (e.g. for Azurite emulator)
}

// Backend stores and retrieves objects in an Azure Blob Storage container.
type Backend struct {
	client        *azblob.Client
	containerName string
	prefix        string
}

// New creates a new Azure Blob Storage backend.
// If AccountKey is set, SharedKeyCredential is used. Otherwise DefaultAzureCredential is used.
// If Endpoint is set it is used as the service URL; otherwise the standard
// https://<accountName>.blob.core.windows.net/ endpoint is used.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.AccountName == "" {
		return nil, fmt.Errorf("azure storage: account_name is required")
	}
	if cfg.ContainerName == "" {
		return nil, fmt.Errorf("azure storage: container_name is required")
	}

	serviceURL := cfg.Endpoint
	if serviceURL == "" {
		serviceURL = fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.AccountName)
	}

	var (
		client *azblob.Client
		err    error
	)

	if cfg.AccountKey != "" {
		cred, credErr := azblob.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
		if credErr != nil {
			return nil, fmt.Errorf("azure storage: failed to create shared key credential: %w", credErr)
		}
		client, err = azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	} else {
		cred, credErr := azidentity.NewDefaultAzureCredential(nil)
		if credErr != nil {
			return nil, fmt.Errorf("azure storage: failed to create default credential: %w", credErr)
		}
		client, err = azblob.NewClient(serviceURL, cred, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("azure storage: failed to create client: %w", err)
	}

	return &Backend{
		client:        client,
		containerName: cfg.ContainerName,
		prefix:        cfg.Prefix,
	}, nil
}

// blobKey returns the full blob key for the given logical path, incorporating
// the configured prefix.
func (b *Backend) blobKey(path string) string {
	if b.prefix == "" {
		return path
	}
	return strings.TrimPrefix(b.prefix+"/"+path, "/")
}

// Put writes data to the given path in the Azure container.
func (b *Backend) Put(ctx context.Context, path string, data []byte) error {
	key := b.blobKey(path)
	_, err := b.client.UploadBuffer(ctx, b.containerName, key, data, nil)
	if err != nil {
		return fmt.Errorf("azure storage: failed to upload blob %q: %w", key, err)
	}
	return nil
}

// Get retrieves the full contents of the blob at the given path.
func (b *Backend) Get(ctx context.Context, path string) ([]byte, error) {
	key := b.blobKey(path)
	resp, err := b.client.DownloadStream(ctx, b.containerName, key, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ResourceNotFound) {
			return nil, fmt.Errorf("azure storage: blob not found: %s", path)
		}
		return nil, fmt.Errorf("azure storage: failed to download blob %q: %w", key, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("azure storage: failed to read blob %q: %w", key, err)
	}
	return data, nil
}

// Delete removes the blob at the given path.
func (b *Backend) Delete(ctx context.Context, path string) error {
	key := b.blobKey(path)
	_, err := b.client.DeleteBlob(ctx, b.containerName, key, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ResourceNotFound) {
			return nil // already gone
		}
		return fmt.Errorf("azure storage: failed to delete blob %q: %w", key, err)
	}
	return nil
}

// List returns blob paths whose names have the given prefix (combined with the
// backend's configured prefix).
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	fullPrefix := b.blobKey(prefix)
	opts := &azblob.ListBlobsFlatOptions{}
	if fullPrefix != "" {
		opts.Prefix = &fullPrefix
	}

	pager := b.client.NewListBlobsFlatPager(b.containerName, opts)
	var results []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure storage: failed to list blobs with prefix %q: %w", fullPrefix, err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item != nil && item.Name != nil {
				results = append(results, *item.Name)
			}
		}
	}
	return results, nil
}

// Exists reports whether a blob exists at the given path.
func (b *Backend) Exists(ctx context.Context, path string) (bool, error) {
	key := b.blobKey(path)
	blobClient := b.client.ServiceClient().NewContainerClient(b.containerName).NewBlobClient(key)
	_, err := blobClient.GetProperties(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ResourceNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("azure storage: failed to check existence of blob %q: %w", key, err)
	}
	return true, nil
}

// Reader returns an io.ReadCloser for streaming reads of the blob at the given path.
func (b *Backend) Reader(ctx context.Context, path string) (io.ReadCloser, error) {
	key := b.blobKey(path)
	resp, err := b.client.DownloadStream(ctx, b.containerName, key, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ResourceNotFound) {
			return nil, fmt.Errorf("azure storage: blob not found: %s", path)
		}
		return nil, fmt.Errorf("azure storage: failed to download blob %q: %w", key, err)
	}
	return resp.Body, nil
}
