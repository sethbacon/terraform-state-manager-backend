package ado

import (
	"context"
	"fmt"
)

// ServiceConnection represents an Azure DevOps service connection (service
// endpoint). Only identifying metadata is captured; credentials held by the
// connection are never read.
type ServiceConnection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// ListServiceConnections returns all service connections in the configured
// project. It calls GET {org}/{project}/_apis/serviceendpoint/endpoints.
func (c *Client) ListServiceConnections(ctx context.Context) ([]ServiceConnection, error) {
	var resp listEnvelope[ServiceConnection]
	path := c.projectPath("_apis/serviceendpoint/endpoints")
	if err := c.httpClient.GetJSON(ctx, path, defaultParams(), &resp); err != nil {
		return nil, fmt.Errorf("listing service connections: %w", err)
	}
	return resp.Value, nil
}
