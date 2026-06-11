package statesource

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

// azureConn reads state from an Azure Blob Storage container using a shared
// account key.
type azureConn struct {
	client    *azblob.Client
	container string
	prefix    string
}

func newAzure(config, creds map[string]any) (*azureConn, error) {
	account, _ := config["account"].(string)
	containerName, _ := config["container"].(string)
	if account == "" || containerName == "" {
		return nil, fmt.Errorf("azureblob source requires config.account and config.container")
	}
	prefix, _ := config["prefix"].(string)
	accountKey, _ := creds["account_key"].(string)
	if accountKey == "" {
		return nil, fmt.Errorf("azureblob source requires credentials.account_key")
	}

	cred, err := azblob.NewSharedKeyCredential(account, accountKey)
	if err != nil {
		return nil, fmt.Errorf("invalid Azure storage credentials: %w", err)
	}
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", account)
	client, err := azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure blob client: %w", err)
	}
	return &azureConn{client: client, container: containerName, prefix: prefix}, nil
}

// coverage:skip:requires-cloud
func (a *azureConn) List(ctx context.Context) ([]StateRef, error) {
	opts := &container.ListBlobsFlatOptions{}
	if a.prefix != "" {
		opts.Prefix = &a.prefix
	}
	var refs []StateRef
	pager := a.client.NewListBlobsFlatPager(a.container, opts)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range page.Segment.BlobItems {
			if b == nil || b.Name == nil || !strings.HasSuffix(*b.Name, ".tfstate") {
				continue
			}
			ref := StateRef{Key: *b.Name, Name: *b.Name}
			if b.Properties != nil {
				if b.Properties.ContentLength != nil {
					ref.Size = *b.Properties.ContentLength
				}
				ref.LastModified = b.Properties.LastModified
			}
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// coverage:skip:requires-cloud
func (a *azureConn) Read(ctx context.Context, key string) (*RawState, error) {
	resp, err := a.client.DownloadStream(ctx, a.container, key, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob %q: %w", key, err)
	}
	body := resp.Body
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	return &RawState{Key: key, Data: data, Size: int64(len(data)), LastModified: resp.LastModified}, nil
}

// coverage:skip:requires-cloud
func (a *azureConn) Write(ctx context.Context, key string, data []byte) error {
	if _, err := a.client.UploadBuffer(ctx, a.container, key, data, nil); err != nil {
		return fmt.Errorf("failed to write blob %q: %w", key, err)
	}
	return nil
}
