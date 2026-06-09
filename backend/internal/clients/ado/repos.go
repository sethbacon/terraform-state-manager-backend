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

// createRepositoryRequest is the body for creating a Git repository. The target
// project is identified by name (Azure DevOps accepts either an id or a name in
// the nested project reference).
type createRepositoryRequest struct {
	Name    string             `json:"name"`
	Project projectReferenceID `json:"project"`
}

// projectReferenceID is the minimal nested project reference used in create
// request bodies.
type projectReferenceID struct {
	Name string `json:"name"`
}

// CreateRepository creates a new empty Git repository named name in the
// configured (target) project. It calls
// POST {org}/{project}/_apis/git/repositories and returns the created
// repository. A 409 Conflict (repository already exists) is surfaced as an
// *APIError; callers can detect it via IsConflict to remain idempotent.
func (c *Client) CreateRepository(ctx context.Context, name string) (*Repository, error) {
	body := createRepositoryRequest{
		Name:    name,
		Project: projectReferenceID{Name: c.config.Project},
	}
	var created Repository
	path := c.projectPath("_apis/git/repositories")
	if err := c.postJSON(ctx, path, defaultParams(), body, &created); err != nil {
		return nil, fmt.Errorf("creating repository %q: %w", name, err)
	}
	return &created, nil
}
