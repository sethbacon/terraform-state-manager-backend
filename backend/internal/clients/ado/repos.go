package ado

import (
	"context"
	"fmt"
)

// Repository represents an Azure DevOps Git repository.
type Repository struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"`
	RemoteURL     string `json:"remoteUrl"`
	SizeBytes     int64  `json:"size"`
}

// ListRepositories returns all Git repositories in the configured project.
// It calls GET {org}/{project}/_apis/git/repositories.
func (c *Client) ListRepositories(ctx context.Context) ([]Repository, error) {
	var resp listEnvelope[Repository]
	path := c.projectPath("_apis/git/repositories")
	if err := c.httpClient.GetJSON(ctx, path, defaultParams(), &resp); err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	return resp.Value, nil
}
