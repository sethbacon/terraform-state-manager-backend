package ado

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// continuationTokenHeader is the response header Azure DevOps uses to signal
// that more pages are available for a paged list endpoint.
// #nosec G101 -- this is a public HTTP response header name, not a credential.
const continuationTokenHeader = "x-ms-continuationtoken"

// Pipeline represents an Azure DevOps pipeline definition.
type Pipeline struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Folder string `json:"folder"`
}

// ListPipelines returns all pipelines in the configured project, following the
// continuationToken paging protocol across multiple pages. It calls
// GET {org}/{project}/_apis/pipelines.
//
// Azure DevOps returns the continuation token in the x-ms-continuationtoken
// response header; when present, it is echoed back as a continuationToken query
// parameter to fetch the next page. Paging stops when the header is absent.
func (c *Client) ListPipelines(ctx context.Context) ([]Pipeline, error) {
	var all []Pipeline
	continuationToken := ""

	for {
		params := defaultParams()
		if continuationToken != "" {
			params.Set("continuationToken", continuationToken)
		}

		path := c.projectPath("_apis/pipelines")
		resp, err := c.httpClient.Get(ctx, path, params)
		if err != nil {
			return nil, fmt.Errorf("listing pipelines: %w", err)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("listing pipelines: unexpected status %d: %s", resp.StatusCode, string(body))
		}

		var envelope listEnvelope[Pipeline]
		decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)
		nextToken := resp.Header.Get(continuationTokenHeader)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding pipelines page: %w", decodeErr)
		}

		all = append(all, envelope.Value...)

		if nextToken == "" {
			break
		}
		continuationToken = nextToken
	}

	return all, nil
}

// CreatePipelineRequest describes a YAML pipeline to define in the target
// project. YAMLPath is the path to the pipeline YAML file within the target
// repository (e.g. "azure-pipelines.yml"); RepositoryID is the id of that
// repository in the target project. Folder is optional and mirrors the source
// pipeline's folder.
type CreatePipelineRequest struct {
	Name         string
	Folder       string
	YAMLPath     string
	RepositoryID string
}

// createPipelineBody is the wire body for POST _apis/pipelines. It defines a
// YAML-backed pipeline whose definition lives in an Azure DevOps Git repository.
type createPipelineBody struct {
	Name          string                  `json:"name"`
	Folder        string                  `json:"folder,omitempty"`
	Configuration pipelineConfigurationIn `json:"configuration"`
}

type pipelineConfigurationIn struct {
	Type       string             `json:"type"`
	Path       string             `json:"path"`
	Repository pipelineRepository `json:"repository"`
}

type pipelineRepository struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// CreatePipeline defines a new YAML pipeline in the configured (target) project.
// It calls POST {org}/{project}/_apis/pipelines and returns the created
// pipeline. A 409 Conflict (a pipeline with the same name already exists) is
// surfaced as an *APIError detectable via IsConflict so callers stay idempotent.
func (c *Client) CreatePipeline(ctx context.Context, req CreatePipelineRequest) (*Pipeline, error) {
	body := createPipelineBody{
		Name:   req.Name,
		Folder: req.Folder,
		Configuration: pipelineConfigurationIn{
			Type: "yaml",
			Path: req.YAMLPath,
			Repository: pipelineRepository{
				ID:   req.RepositoryID,
				Type: "azureReposGit",
			},
		},
	}
	var created Pipeline
	path := c.projectPath("_apis/pipelines")
	if err := c.postJSON(ctx, path, defaultParams(), body, &created); err != nil {
		return nil, fmt.Errorf("creating pipeline %q: %w", req.Name, err)
	}
	return &created, nil
}
