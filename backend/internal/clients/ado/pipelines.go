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
