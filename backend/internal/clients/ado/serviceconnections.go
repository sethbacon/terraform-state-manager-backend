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

// AdoptServiceConnectionRequest describes a service connection to recreate
// ("adopt") in the target project. URL is the connection's server URL (e.g. the
// management or Git endpoint). Credentials held by the source connection are
// never read, so the adopted connection is created without authorization data;
// credentials must be re-supplied out of band before the connection is usable.
type AdoptServiceConnectionRequest struct {
	Name string
	Type string
	URL  string
}

// adoptServiceConnectionBody is the wire body for POST
// _apis/serviceendpoint/endpoints. The project reference scopes the new
// connection to the target project.
type adoptServiceConnectionBody struct {
	Name                             string                      `json:"name"`
	Type                             string                      `json:"type"`
	URL                              string                      `json:"url"`
	ServiceEndpointProjectReferences []serviceEndpointProjectRef `json:"serviceEndpointProjectReferences"`
}

type serviceEndpointProjectRef struct {
	Name             string             `json:"name"`
	ProjectReference projectReferenceID `json:"projectReference"`
}

// AdoptServiceConnection recreates a service connection in the configured
// (target) project. It calls POST {org}/_apis/serviceendpoint/endpoints
// (project-scoped via the project reference) and returns the created connection.
// A 409 Conflict is surfaced as an *APIError detectable via IsConflict so
// callers stay idempotent.
func (c *Client) AdoptServiceConnection(ctx context.Context, req AdoptServiceConnectionRequest) (*ServiceConnection, error) {
	body := adoptServiceConnectionBody{
		Name: req.Name,
		Type: req.Type,
		URL:  req.URL,
		ServiceEndpointProjectReferences: []serviceEndpointProjectRef{
			{
				Name:             req.Name,
				ProjectReference: projectReferenceID{Name: c.config.Project},
			},
		},
	}
	var created ServiceConnection
	if err := c.postJSON(ctx, "_apis/serviceendpoint/endpoints", defaultParams(), body, &created); err != nil {
		return nil, fmt.Errorf("adopting service connection %q: %w", req.Name, err)
	}
	return &created, nil
}
